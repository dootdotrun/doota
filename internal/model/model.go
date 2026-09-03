// Package model talks to the language model.
//
// It wraps the OpenAI Go SDK against an OpenAI-compatible endpoint and exposes
// nothing from it. No SDK type appears in any signature here, for the same reason
// internal/sandbox hides the Sprites SDK: the agent loop should be readable and
// testable without knowing which client library is underneath, and swapping the
// library should not be a change to the loop.
//
// # Why the Responses API
//
// This package used to speak Chat Completions. It does not any more, and the reason
// is the single most consequential thing about running a reasoning model in a tool
// loop.
//
// Muse Spark reasons before it emits anything, and that reasoning is the expensive
// part of the call. On Chat Completions there is nowhere to put it: the transport
// has no representation for reasoning state, so it is discarded at the end of every
// request. In a tool loop that means the model thinks its way to a decision, calls a
// tool, and then — on the very next request, with the tool result in hand — starts
// again from nothing. Every turn re-derives the reasoning of the turn before it.
//
// The cost is not subtle. The output budget goes disproportionately to rebuilding
// context the model already had, which makes a budget that should be ample run out
// instead; and multi-step work is done by a model that keeps losing its own thread,
// which reads as carelessness. Meta's documentation names the Responses API as the
// recommended path for tool calling for exactly this reason: it preserves reasoning
// across tool turns.
//
// # Stateless, not previous_response_id
//
// The Responses API offers two ways to keep that continuity. `previous_response_id`
// threads turns server-side. This package deliberately does not use it.
//
// Everything durable in this application lives in Postgres, and a restart resumes a
// run from its last boundary. Server-side conversation state breaks that: a response
// id can expire underneath a paused run, Clear Conversation works by flipping
// in_context rather than by telling a provider anything, and a transcript that can
// only be replayed by quoting opaque remote ids is no longer a transcript this
// application owns.
//
// So: Store is false, Include asks for `reasoning.encrypted_content`, and the
// reasoning items come back to us, get persisted alongside the assistant turn that
// produced them, and are replayed verbatim on the next request. The conversation
// remains reconstructible from the database alone.
//
// # What the live API does
//
// Measured, not assumed. Two behaviours shape the code below:
//
// **The reasoning happens before any output, in silence.** A three-sentence answer
// spent 830 of 935 completion tokens reasoning: several seconds of nothing, then all
// the content at once. A UI that only renders text deltas therefore looks stalled
// for most of every call, which is why Handler has an explicit OnStart and the chat
// screen shows a thinking state.
//
// **The output budget covers reasoning as well as visible output.** It is spent on
// reasoning first, so too small a budget produces a successful call that said
// nothing at all. Unlike Chat Completions — where that case was indistinguishable
// from a normal one, arriving as finish_reason "stop" with no usage chunk — the
// Responses API says so outright: status "incomplete" with an incomplete_details
// reason of max_output_tokens, and a usage breakdown that counts reasoning tokens
// separately. Response.SilentCause can therefore give an honest answer instead of a
// guess.
//
// The budget travels as `max_output_tokens`. On Chat Completions this package sent
// `max_tokens`, the legacy field, which is documented as unsupported on reasoning
// models and was rejected with a bare "the request contains invalid parameters" that
// named nothing.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
)

// MaxOutputTokens is the largest output budget the API will accept.
//
// Exceeding it is a 400 with a body that does not name the offending parameter, so
// it is enforced here and mirrored by a bound on the Settings field. Both are
// needed: the form stops an operator storing an impossible value, and this stops a
// value that predates the bound from bricking every request.
const MaxOutputTokens = 131072

// ContextWindow is how much the model can hold in one request.
//
// Used only to render "how full is the context" as a fraction, which is worth having
// because a window this large feeling exhausted is indistinguishable from one that is
// without a number on screen. It is a display constant, not a limit this code
// enforces — the API enforces its own — so a model with a different window makes the
// denominator wrong and nothing else.
const ContextWindow = 1048576

// Roles, mirroring the message table's check constraint.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Reasoning effort levels, lowest to highest.
//
// Omitting the effort lets the model choose its own depth. Naming them here keeps
// the valid set in one place, since the endpoint rejects anything else and this is
// operator-editable configuration.
const (
	EffortMinimal = "minimal"
	EffortLow     = "low"
	EffortMedium  = "medium"
	EffortHigh    = "high"
	EffortXHigh   = "xhigh"
)

// Efforts is every accepted reasoning effort, in order.
var Efforts = []string{EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh}

// ToolCall is one function invocation the model asked for.
//
// ID is the API's call_id, which is what a result is addressed to. The Responses API
// also gives each call an item id; that is not kept, because nothing needs it and
// storing both invites using the wrong one.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ReasoningItem is one block of the model's retained chain of thought.
//
// Opaque by design. EncryptedContent is not readable by us and is not meant to be;
// it is handed back verbatim so the model can pick up where it left off. Summary is
// occasionally populated with a short human-readable gloss, kept only because
// returning an item without the fields it arrived with is asking for trouble.
//
// Never rendered to the operator. It is state, not a message.
type ReasoningItem struct {
	ID               string   `json:"id"`
	EncryptedContent string   `json:"encrypted_content,omitempty"`
	Summary          []string `json:"summary,omitempty"`
}

// Message is one turn of the conversation as the model sees it.
type Message struct {
	Role      string
	Content   string
	ToolCalls []ToolCall

	// Reasoning is what the model was thinking when it produced this turn, to be
	// replayed on later requests. Only ever set on an assistant turn.
	Reasoning []ReasoningItem

	// ToolCallID ties a RoleTool message back to the call it answers.
	ToolCallID string
}

// ToolSpec advertises one tool.
//
// Parameters is pre-encoded JSON Schema rather than a struct, which keeps this
// package independent of internal/tools: the agent owns the translation, and
// neither package has to know the other's shape.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Usage is the token count for one call.
type Usage struct {
	PromptTokens     int
	CompletionTokens int

	// ReasoningTokens is the part of CompletionTokens spent thinking rather than
	// answering. Reported separately by this transport, and worth keeping: it is the
	// difference between "the model had nothing to say" and "the model thought until
	// the budget ran out", which used to be unanswerable.
	ReasoningTokens int
}

// TotalTokens is the whole call's footprint.
func (u Usage) TotalTokens() int { return u.PromptTokens + u.CompletionTokens }

// Request is one model call.
//
// APIKey and BaseURL travel with the request rather than being fixed when the
// Client is built. They are configuration, editable on the Settings screen, and a
// client that captured them at boot meant an operator could paste a corrected key
// into the form and watch every subsequent call keep using the old one. Client
// caches the underlying SDK client so passing them per call costs nothing until
// they actually change.
type Request struct {
	APIKey    string
	BaseURL   string
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int

	// ReasoningEffort is one of Efforts, or empty to let the model decide.
	ReasoningEffort string
}

// Response is a completed call.
type Response struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
	Model        string
	Latency      time.Duration

	// Reasoning is the chain of thought to hand back on the next request. Persisting
	// this and replaying it is the whole point of being on this transport.
	Reasoning []ReasoningItem

	// Truncated means the API said the output budget ran out. Reliable here: it comes
	// from an explicit incomplete_details reason rather than being inferred.
	Truncated bool

	// Filtered means the response was cut short by a content filter rather than by
	// the budget. A different problem with a different remedy, and telling an
	// operator to raise a limit would send them somewhere useless.
	Filtered bool

	// UsageReported is false when the API sent no usage at all.
	UsageReported bool
}

// Silent reports a call that came back with nothing to say and nothing to do — no
// content and no tool calls. The loop has to notice rather than treating it as a
// reply, or it presents an empty message as an answer.
func (r *Response) Silent() bool {
	return strings.TrimSpace(r.Content) == "" && len(r.ToolCalls) == 0
}

// SilentCause explains a response that came back with nothing to act on.
//
// The causes are deliberately kept apart. This replaced a single Starved() predicate
// defined as "silent, and either truncated or missing usage", which collapsed every
// empty stream — a dropped connection, a refusal — into one report claiming the
// output budget had been consumed by reasoning.
//
// That mattered more than a wrong log line. The remedy printed alongside it was
// "raise Max output tokens", so the one failure the operator could not fix that way
// was the one they were told to fix that way, and raising the value past the API
// ceiling turns every later request into a 400. A cause that is not known has to say
// so.
type SilentCause string

const (
	// NotSilent means the response had content or tool calls.
	NotSilent SilentCause = ""

	// SilentTruncated is the API stating outright that the budget ran out.
	SilentTruncated SilentCause = "truncated"

	// SilentFiltered is a content filter, not a budget.
	SilentFiltered SilentCause = "filtered"

	// SilentReasoning is tokens billed with nothing delivered: the budget was
	// real, it was spent, and none of it reached us as content or a tool call.
	SilentReasoning SilentCause = "reasoning"

	// SilentUnknown is nothing back and no accounting for it. Usually a stream
	// that died or a request that was refused — not a budget problem, and saying
	// otherwise sends the operator to the wrong setting.
	SilentUnknown SilentCause = "unknown"
)

// SilentCause classifies an empty response.
func (r *Response) SilentCause() SilentCause {
	switch {
	case !r.Silent():
		return NotSilent
	case r.Filtered:
		return SilentFiltered
	case r.Truncated:
		return SilentTruncated
	case r.UsageReported && r.Usage.CompletionTokens > 0:
		return SilentReasoning
	default:
		return SilentUnknown
	}
}

// Handler receives progress while a call is in flight. Every field may be nil.
type Handler struct {
	// OnStart fires once the request is accepted, before any output. On this model
	// that is the beginning of a silence of several seconds, so it is what the UI
	// hangs its thinking indicator on.
	OnStart func()

	// OnText fires for each content delta, in order.
	OnText func(delta string)

	// OnToolCall fires when a tool call's name is known, before its arguments have
	// finished arriving. It is for showing "calling read_file…", not for executing:
	// execution needs the complete arguments from Response.
	OnToolCall func(name string)
}

func (h Handler) start() {
	if h.OnStart != nil {
		h.OnStart()
	}
}

func (h Handler) text(delta string) {
	if h.OnText != nil && delta != "" {
		h.OnText(delta)
	}
}

func (h Handler) toolCall(name string) {
	if h.OnToolCall != nil && name != "" {
		h.OnToolCall(name)
	}
}

// Client is a model endpoint.
//
// The SDK client underneath is built lazily from the credentials on the first
// request and reused until they change, so the common case is one construction
// for the life of the process and an edit in Settings takes effect on the next
// call without a restart.
type Client struct {
	log *slog.Logger

	mu      sync.Mutex
	api     openai.Client
	apiKey  string
	baseURL string
	built   bool
}

// New builds a client for an OpenAI-compatible endpoint.
func New(log *slog.Logger) *Client {
	return &Client{log: log}
}

// resolve returns an SDK client for these credentials, rebuilding only when they
// differ from the cached pair.
func (c *Client) resolve(apiKey, baseURL string) openai.Client {
	apiKey = strings.TrimSpace(apiKey)
	baseURL = strings.TrimSpace(baseURL)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.built && c.apiKey == apiKey && c.baseURL == baseURL {
		return c.api
	}

	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	c.api = openai.NewClient(opts...)
	c.apiKey, c.baseURL, c.built = apiKey, baseURL, true
	if c.log != nil {
		c.log.Info("model client built", "base_url", baseURL)
	}
	return c.api
}

// streamTimeout bounds a single model call.
//
// Generous, because a reasoning model working through a large context is slow by
// design. It exists so a hung connection cannot park the loop forever, not to
// discipline the model.
const streamTimeout = 10 * time.Minute

// Stream makes one streaming call and returns the assembled result.
//
// Streaming is not for show. The alternative is a phone showing nothing for the
// length of a long call, and the loop needing a timeout long enough to cover the
// worst case with no way to tell slow from dead.
//
// Assembly is simpler than it was on Chat Completions. There, tool calls had to be
// accumulated by index across chunks, because the first chunk for an index carried
// the id and name and later ones appended argument fragments. Here the terminal
// `response.completed` event carries the finished response — every output item, in
// order, with usage — so the deltas are only for the live view and the result is
// read from one authoritative payload.
func (c *Client) Stream(ctx context.Context, req Request, h Handler) (*Response, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("model name is empty")
	}
	// Checked here rather than left to the endpoint, because a missing key comes
	// back as an opaque 401 and this is the single most likely thing to be wrong on
	// a fresh install.
	if strings.TrimSpace(req.APIKey) == "" {
		return nil, fmt.Errorf("no model API key is configured: set one on the Settings screen")
	}

	params, err := c.params(req)
	if err != nil {
		return nil, err
	}

	api := c.resolve(req.APIKey, req.BaseURL)

	ctx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()

	started := time.Now()
	stream := api.Responses.NewStreaming(ctx, params)
	defer stream.Close()

	h.start()

	resp := &Response{Model: req.Model}
	var text strings.Builder
	// Tool call names are announced as their items appear, so the UI can say what is
	// being called before the arguments have finished arriving.
	announced := map[string]bool{}
	var final *responses.Response
	var streamErr error

	for stream.Next() {
		event := stream.Current()

		switch event.Type {
		case "response.output_text.delta":
			if delta := event.Delta.OfString; delta != "" {
				text.WriteString(delta)
				h.text(delta)
			}

		case "response.output_item.added":
			if event.Item.Type == "function_call" && !announced[event.Item.ID] {
				announced[event.Item.ID] = true
				h.toolCall(event.Item.Name)
			}

		case "response.completed", "response.incomplete":
			done := event.Response
			final = &done

		case "response.failed":
			done := event.Response
			final = &done
			if done.Error.Message != "" {
				streamErr = fmt.Errorf("model reported failure: %s", done.Error.Message)
			} else {
				streamErr = fmt.Errorf("model reported failure with no detail")
			}

		case "error":
			// A transport-level error frame. The message is the only useful part.
			if event.Message != "" {
				streamErr = fmt.Errorf("model stream error: %s", event.Message)
			} else {
				streamErr = fmt.Errorf("model stream error with no detail")
			}
		}
	}

	resp.Content = text.String()
	resp.Latency = time.Since(started)

	if err := stream.Err(); err != nil {
		// Partial output is returned alongside the error: the loop persists what
		// arrived and marks it interrupted, so a stream that dies late does not
		// silently lose the model's work.
		return resp, fmt.Errorf("model stream: %w", err)
	}
	if streamErr != nil {
		if final != nil {
			c.absorb(resp, final)
		}
		return resp, streamErr
	}
	if final == nil {
		// The stream ended without a terminal event. Whatever text arrived is
		// returned, and the caller treats a silent response on its merits.
		return resp, fmt.Errorf("model stream ended without completing")
	}

	c.absorb(resp, final)

	c.log.Info("model call",
		"model", resp.Model,
		"finish_reason", resp.FinishReason,
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
		"reasoning_tokens", resp.Usage.ReasoningTokens,
		"tool_calls", len(resp.ToolCalls),
		"reasoning_items", len(resp.Reasoning),
		"truncated", resp.Truncated,
		"latency_ms", resp.Latency.Milliseconds(),
	)
	return resp, nil
}

// absorb reads the terminal response payload into our own shape.
//
// Content is taken from the accumulated deltas rather than from the output items:
// they agree, and preferring the deltas means a response whose terminal event is
// malformed still returns the text the operator already watched arrive.
func (c *Client) absorb(resp *Response, final *responses.Response) {
	if final.Model != "" {
		resp.Model = final.Model
	}
	resp.FinishReason = string(final.Status)

	switch final.IncompleteDetails.Reason {
	case "max_output_tokens":
		resp.Truncated = true
	case "content_filter":
		resp.Filtered = true
	}

	if final.Usage.TotalTokens > 0 || final.Usage.InputTokens > 0 {
		resp.UsageReported = true
		resp.Usage = Usage{
			PromptTokens:     int(final.Usage.InputTokens),
			CompletionTokens: int(final.Usage.OutputTokens),
			ReasoningTokens:  int(final.Usage.OutputTokensDetails.ReasoningTokens),
		}
	}

	for _, item := range final.Output {
		switch item.Type {
		case "reasoning":
			r := ReasoningItem{ID: item.ID, EncryptedContent: item.EncryptedContent}
			for _, s := range item.Summary {
				if s.Text != "" {
					r.Summary = append(r.Summary, s.Text)
				}
			}
			// An item with neither an encrypted blob nor a summary carries nothing to
			// replay, and sending a hollow one back is a request the API may reject.
			if r.EncryptedContent != "" || len(r.Summary) > 0 {
				resp.Reasoning = append(resp.Reasoning, r)
			}

		case "function_call":
			args := json.RawMessage(strings.TrimSpace(item.Arguments))
			if len(args) == 0 {
				// An argument-less call is legal for a tool with no required fields, and
				// an empty object is what every decoder downstream expects.
				args = json.RawMessage("{}")
			}
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{ID: item.CallID, Name: item.Name, Args: args})

		case "message":
			// Deltas already gave us this. Used only as a fallback for a response that
			// somehow delivered no deltas at all.
			if strings.TrimSpace(resp.Content) == "" {
				var b strings.Builder
				for _, part := range item.Content {
					b.WriteString(part.Text)
				}
				resp.Content = b.String()
			}
		}
	}
}

// params converts a Request into the SDK's shape.
func (c *Client) params(req Request) (responses.ResponseNewParams, error) {
	input, err := inputItems(req.Messages)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}

	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(req.Model),
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: input},

		// The reasoning blobs have to come back to us or there is nothing to replay,
		// and nothing is kept server-side. See the package doc on why not
		// previous_response_id.
		Include: []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},
		Store:   param.NewOpt(false),
	}

	// The system prompt is instructions rather than an input item. Kept out of the
	// item list so it cannot be mistaken for a turn, and so the whole prompt can be
	// swapped between requests without rewriting history.
	if s := strings.TrimSpace(req.System); s != "" {
		params.Instructions = param.NewOpt(s)
	}

	// Clamped as well as bounded at the form, because the ceiling is otherwise only
	// discoverable as an opaque 400 and a value stored before the bound existed
	// would keep failing.
	if req.MaxTokens > 0 {
		budget := req.MaxTokens
		if budget > MaxOutputTokens {
			budget = MaxOutputTokens
		}
		params.MaxOutputTokens = param.NewOpt(int64(budget))
	}

	if effort := strings.TrimSpace(req.ReasoningEffort); effort != "" {
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(effort)}
	}

	for _, t := range req.Tools {
		var schema map[string]any
		if len(t.Parameters) > 0 {
			if err := json.Unmarshal(t.Parameters, &schema); err != nil {
				return responses.ResponseNewParams{}, fmt.Errorf("tool %s has an invalid parameter schema: %w", t.Name, err)
			}
		}
		fn := responses.FunctionToolParam{
			Name:       t.Name,
			Parameters: schema,
			// Strict schema validation is off because these schemas are hand-written for
			// a model to read, and strict mode additionally requires every property to be
			// listed as required — which would make every optional tool argument
			// mandatory.
			Strict: param.NewOpt(false),
		}
		if t.Description != "" {
			fn.Description = param.NewOpt(t.Description)
		}
		params.Tools = append(params.Tools, responses.ToolUnionParam{OfFunction: &fn})
	}

	return params, nil
}

// inputItems flattens the conversation into the API's item list.
//
// One message can become several items, and the order within a turn matters: the
// reasoning that produced a decision has to precede the decision. So an assistant
// turn expands to its reasoning items, then its text, then its tool calls.
func inputItems(messages []Message) ([]responses.ResponseInputItemUnionParam, error) {
	out := make([]responses.ResponseInputItemUnionParam, 0, len(messages)+4)

	for _, m := range messages {
		switch m.Role {
		case RoleUser:
			out = append(out, responses.ResponseInputItemParamOfMessage(m.Content, responses.EasyInputMessageRoleUser))

		case RoleSystem:
			out = append(out, responses.ResponseInputItemParamOfMessage(m.Content, responses.EasyInputMessageRoleSystem))

		case RoleAssistant:
			for _, r := range m.Reasoning {
				summary := make([]responses.ResponseReasoningItemSummaryParam, 0, len(r.Summary))
				for _, s := range r.Summary {
					summary = append(summary, responses.ResponseReasoningItemSummaryParam{Text: s})
				}
				item := responses.ResponseInputItemParamOfReasoning(r.ID, summary)
				if r.EncryptedContent != "" {
					item.OfReasoning.EncryptedContent = param.NewOpt(r.EncryptedContent)
				}
				out = append(out, item)
			}
			if strings.TrimSpace(m.Content) != "" {
				out = append(out, responses.ResponseInputItemParamOfMessage(m.Content, responses.EasyInputMessageRoleAssistant))
			}
			// An assistant turn that called tools has to carry the calls back, or the
			// tool results that follow have nothing to attach to and the API rejects
			// the request.
			for _, tc := range m.ToolCalls {
				args := string(tc.Args)
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				out = append(out, responses.ResponseInputItemParamOfFunctionCall(args, tc.ID, tc.Name))
			}

		case RoleTool:
			out = append(out, responses.ResponseInputItemParamOfFunctionCallOutput(m.ToolCallID, m.Content))

		default:
			return nil, fmt.Errorf("unknown message role %q", m.Role)
		}
	}
	return out, nil
}
