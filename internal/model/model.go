// Package model talks to the language model.
//
// It wraps the OpenAI Go SDK against an OpenAI-compatible endpoint and exposes
// nothing from it. No SDK type appears in any signature here, for the same reason
// internal/sandbox hides the Sprites SDK: the agent loop should be readable and
// testable without knowing which client library is underneath, and swapping the
// library should not be a change to the loop.
//
// # What the live API actually does
//
// Measured against Muse Spark 1.2 on the Meta Model API, not assumed. Three
// behaviours shape everything in this package:
//
// **It is a reasoning model, and the reasoning is invisible.** Usage comes back
// with a large `reasoning_tokens` count, but the reasoning text is returned
// nowhere — not on the non-streaming message, not as a stream delta. So there is
// nothing to replay between turns. That closes an open question in the README: the
// answer is not "the API rejects replayed reasoning", it is "there is no reasoning
// content to replay". Extra fields sent on an assistant message are accepted and
// silently ignored, so nothing breaks either way.
//
// **The reasoning happens before any output, in silence.** A three-sentence answer
// spent 830 of 935 completion tokens reasoning: 5.6 seconds of nothing, then all
// the content in 0.2 seconds across seven chunks. A UI that only renders text
// deltas therefore looks stalled for most of every call, which is why Handler has
// an explicit OnStart and the chat screen shows a thinking state.
//
// **The output budget has to cover reasoning as well as output, and running out
// is disguised.** The budget is spent on reasoning first, so too small a budget
// produces a successful call that said nothing. Worse, the two transports disagree
// about how they say so:
//
//	non-streaming   finish_reason "length", content null, usage reported
//	streaming       finish_reason "stop",   no content,   NO usage chunk at all
//
// So in streaming mode — which is the only mode this package uses — a starved call
// can be indistinguishable from a normal one by finish_reason. Response.SilentCause
// classifies what actually happened, because the alternative is a loop that spends
// real money, records $0, and tells the operator the model had nothing to say.
//
// The budget travels as `max_completion_tokens`. `max_tokens` is the legacy field
// and is documented as unsupported on reasoning models — sending it is a 400 with
// an opaque "invalid parameters" body, which is close to undiagnosable from the
// operator's side.
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
	"github.com/openai/openai-go/shared"
	"github.com/openai/openai-go/shared/constant"
)

// MaxOutputTokens is the largest output budget the API will accept.
//
// Exceeding it is a 400 with a body that does not name the offending parameter, so
// it is enforced here and mirrored by a bound on the Settings field. Both are
// needed: the form stops an operator storing an impossible value, and this stops a
// value that predates the bound from bricking every request.
const MaxOutputTokens = 131072

// Roles, mirroring the message table's check constraint.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Finish reasons worth naming.
const (
	FinishStop      = "stop"
	FinishToolCalls = "tool_calls"
	FinishLength    = "length"
)

// ToolCall is one function invocation the model asked for.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// Message is one turn of the conversation as the model sees it.
type Message struct {
	Role      string
	Content   string
	ToolCalls []ToolCall

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
//
// Kept after cost accounting was removed because UsageReported — the fact that
// the API said anything at all about tokens — is what Starved branches on, and
// the counts themselves are useful in the call log.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
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
}

// Response is a completed call.
type Response struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
	Model        string
	Latency      time.Duration

	// Truncated means the API said the output budget ran out.
	//
	// Reliable on the non-streaming transport and effectively never set on the
	// streaming one, which reports "stop" regardless. Kept because it is correct
	// where it is reported and other providers do set it — but Starved is what to
	// branch on here.
	Truncated bool

	// UsageReported is false when the API sent no usage at all, which it does when
	// the output budget is exhausted mid-reasoning. Distinguishes "this call was
	// free" from "we were not told what this call cost".
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
// Three causes, deliberately kept apart. This replaced a single Starved() predicate
// defined as "silent, and either truncated or missing usage", which collapsed every
// empty stream — a dropped connection, a refusal, a rejected request — into one
// report claiming the output budget had been consumed by reasoning.
//
// That mattered more than a wrong log line. The remedy printed alongside it was
// "raise Max output tokens", so the one failure the operator could not actually fix
// that way was the one they were told to fix that way, and raising the value past
// the API's ceiling turns every later request into a 400. A cause that is not known
// has to say so.
type SilentCause string

const (
	// NotSilent means the response had content or tool calls.
	NotSilent SilentCause = ""

	// SilentTruncated is the API stating outright that the budget ran out.
	SilentTruncated SilentCause = "truncated"

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
//
// The SDK's own retry handling is left at its default. Proper backoff with jitter
// across the whole loop is Phase 5's job, and stacking a bespoke retry on top of
// an SDK retry now would make the eventual attempt budget hard to reason about.
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
	stream := api.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	h.start()

	var content strings.Builder
	// Tool calls accumulate by the index the API assigns, which is how parallel
	// calls stay separate: the first chunk for an index carries the id and name,
	// later ones append argument fragments. Accumulated here rather than with the
	// SDK's helper because the wire shape was verified directly and this is the one
	// piece of the protocol the whole tool loop rests on.
	type pending struct {
		id, name  string
		args      strings.Builder
		announced bool
	}
	calls := map[int64]*pending{}
	var order []int64

	resp := &Response{Model: req.Model}

	for stream.Next() {
		chunk := stream.Current()

		if chunk.Usage.TotalTokens > 0 || chunk.Usage.PromptTokens > 0 {
			resp.UsageReported = true
			resp.Usage = Usage{
				PromptTokens:     int(chunk.Usage.PromptTokens),
				CompletionTokens: int(chunk.Usage.CompletionTokens),
			}
		}
		if chunk.Model != "" {
			resp.Model = chunk.Model
		}

		for _, choice := range chunk.Choices {
			if delta := choice.Delta.Content; delta != "" {
				content.WriteString(delta)
				h.text(delta)
			}
			for _, tc := range choice.Delta.ToolCalls {
				p, seen := calls[tc.Index]
				if !seen {
					p = &pending{}
					calls[tc.Index] = p
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					p.args.WriteString(tc.Function.Arguments)
				}
				if !p.announced && p.name != "" {
					p.announced = true
					h.toolCall(p.name)
				}
			}
			if choice.FinishReason != "" {
				resp.FinishReason = choice.FinishReason
			}
		}
	}

	if err := stream.Err(); err != nil {
		// Partial output is returned to the caller in the error path's place: the
		// loop persists what arrived and marks it interrupted, so a stream that
		// dies late does not silently lose the model's work.
		return &Response{
			Content:      content.String(),
			FinishReason: resp.FinishReason,
			Usage:        resp.Usage,
			Model:        resp.Model,
			Latency:      time.Since(started),
		}, fmt.Errorf("model stream: %w", err)
	}

	for _, idx := range order {
		p := calls[idx]
		if p.name == "" {
			continue
		}
		args := json.RawMessage(strings.TrimSpace(p.args.String()))
		if len(args) == 0 {
			// An argument-less call is legal for a tool with no required fields, and
			// an empty object is what every decoder downstream expects.
			args = json.RawMessage("{}")
		}
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{ID: p.id, Name: p.name, Args: args})
	}

	resp.Content = content.String()
	resp.Latency = time.Since(started)
	resp.Truncated = resp.FinishReason == FinishLength

	c.log.Info("model call",
		"model", resp.Model,
		"finish_reason", resp.FinishReason,
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
		"tool_calls", len(resp.ToolCalls),
		"latency_ms", resp.Latency.Milliseconds(),
	)
	return resp, nil
}

// params converts a Request into the SDK's shape.
func (c *Client) params(req Request) (openai.ChatCompletionNewParams, error) {
	msgs := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)
	if s := strings.TrimSpace(req.System); s != "" {
		msgs = append(msgs, openai.SystemMessage(s))
	}

	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			msgs = append(msgs, openai.UserMessage(m.Content))

		case RoleAssistant:
			if len(m.ToolCalls) == 0 {
				msgs = append(msgs, openai.AssistantMessage(m.Content))
				break
			}
			// An assistant turn that called tools has to carry the calls back, or the
			// tool results that follow it have nothing to attach to and the API
			// rejects the request.
			assistant := openai.ChatCompletionAssistantMessageParam{
				ToolCalls: make([]openai.ChatCompletionMessageToolCallParam, 0, len(m.ToolCalls)),
			}
			if m.Content != "" {
				assistant.Content.OfString = param.NewOpt(m.Content)
			}
			for _, tc := range m.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallParam{
					ID:   tc.ID,
					Type: constant.Function("function"),
					Function: openai.ChatCompletionMessageToolCallFunctionParam{
						Name:      tc.Name,
						Arguments: string(tc.Args),
					},
				})
			}
			msgs = append(msgs, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})

		case RoleTool:
			msgs = append(msgs, openai.ToolMessage(m.Content, m.ToolCallID))

		case RoleSystem:
			msgs = append(msgs, openai.SystemMessage(m.Content))

		default:
			return openai.ChatCompletionNewParams{}, fmt.Errorf("unknown message role %q", m.Role)
		}
	}

	params := openai.ChatCompletionNewParams{
		Model:         shared.ChatModel(req.Model),
		Messages:      msgs,
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: param.NewOpt(true)},
	}
	// max_completion_tokens, never max_tokens. The latter is the legacy field, is
	// documented as unsupported on reasoning models, and this endpoint rejects it
	// with a bare "The request contains invalid parameters" — no mention of which
	// parameter. Clamped as well as named, because the ceiling is likewise only
	// discoverable as that same opaque 400.
	if req.MaxTokens > 0 {
		budget := req.MaxTokens
		if budget > MaxOutputTokens {
			budget = MaxOutputTokens
		}
		params.MaxCompletionTokens = param.NewOpt(int64(budget))
	}

	for _, t := range req.Tools {
		var schema map[string]any
		if len(t.Parameters) > 0 {
			if err := json.Unmarshal(t.Parameters, &schema); err != nil {
				return openai.ChatCompletionNewParams{}, fmt.Errorf("tool %s has an invalid parameter schema: %w", t.Name, err)
			}
		}
		params.Tools = append(params.Tools, openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        t.Name,
				Description: param.NewOpt(t.Description),
				Parameters:  shared.FunctionParameters(schema),
			},
		})
	}
	return params, nil
}
