// Package agent runs the conversation loop.
//
// The loop is deliberately small. One model call, then its tools, then repeat
// until the model stops asking for tools. Everything durable lives in Postgres so
// a restart resumes from the last boundary; everything else is a local goroutine
// that can be killed without consequence.
//
// What this package used to contain, and no longer does: a 45-second lease per run
// with a renewal goroutine, a reconciler ticker scanning for expired leases every
// five seconds forever, a drain protocol so a replacement machine could take over
// mid-run, a two-stage pause, and a goal/phase state machine that injected synthetic
// system messages to push the model through a contract it had to satisfy in a fixed
// order. That was infrastructure for a fleet serving many users. This serves one.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/dootdotrun/doot-ai/internal/events"
	"github.com/dootdotrun/doot-ai/internal/model"
	"github.com/dootdotrun/doot-ai/internal/project"
	"github.com/dootdotrun/doot-ai/internal/sandbox"
	"github.com/dootdotrun/doot-ai/internal/store"
	"github.com/dootdotrun/doot-ai/internal/tools"
)

// A single model call is bounded by internal/model, which applies its own timeout
// to every request. There used to be a 30-minute one here as well, wrapped around
// the 10-minute one underneath: the inner deadline always won, so the outer number
// described a limit that had never once been reached. The cancel this needs is for
// Pause, not for a deadline.
const (
	deltaFlush  = 100 * time.Millisecond
	maxAttempts = 4
	maxBackoff  = 12 * time.Second
)

// Agent states published to the live UI.
const (
	StateIdle     = "idle"
	StateThinking = "thinking"
	StateWorking  = "working"
	StateError    = "error"

	// StateDone marks a run that reached a terminal state on purpose, as opposed
	// to one that was paused, errored, or never started.
	//
	// It exists because "idle" was doing both jobs and the UI renders idle as the
	// absence of a bar. Finishing therefore looked exactly like nothing having
	// happened, which is the whole of "the agent completes the task but there is
	// no visual clarity that it is done".
	StateDone = "done"
)

// ErrBusy is returned when a run is already active for the project.
var ErrBusy = errors.New("the agent is already working")

// worker holds the cancels for one in-flight run, so a pause can stop the model
// stream and the running tool. Purely process-local: the durable pause lives in
// the run row, and this is only how it takes effect immediately.
type worker struct {
	mu           sync.Mutex
	streamCancel context.CancelFunc
	toolCancel   context.CancelFunc
}

func (w *worker) setStream(c context.CancelFunc) { w.mu.Lock(); w.streamCancel = c; w.mu.Unlock() }
func (w *worker) setTool(c context.CancelFunc)   { w.mu.Lock(); w.toolCancel = c; w.mu.Unlock() }
func (w *worker) cancelAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.streamCancel != nil {
		w.streamCancel()
	}
	if w.toolCancel != nil {
		w.toolCancel()
	}
}

// Service runs the loop.
type Service struct {
	store    *store.Store
	projects *project.Service
	model    *model.Client
	tools    *tools.Registry
	reviewer *tools.Registry
	shipping *tools.Registry
	hub      *events.Hub
	log      *slog.Logger

	mu      sync.Mutex
	workers map[string]*worker // keyed by run id
	wg      sync.WaitGroup
}

func New(st *store.Store, projects *project.Service, client *model.Client,
	registry, reviewer *tools.Registry, hub *events.Hub, log *slog.Logger) *Service {
	return &Service{
		store: st, projects: projects, model: client, tools: registry, reviewer: reviewer,
		shipping: tools.Shipping(),
		hub:      hub, log: log,
		workers: map[string]*worker{},
	}
}

// Busy is a durable query, not a process-local goroutine check.
func (s *Service) Busy(projectID string) bool {
	r, err := s.store.ActiveRun(context.Background(), projectID)
	return err == nil && r.State == store.RunRunning
}

func (s *Service) ActiveRun(ctx context.Context, projectID string) (*store.Run, error) {
	return s.store.ActiveRun(ctx, projectID)
}

// Submit creates a run for a new request, or turns an awaiting-human answer into
// an ordinary user message and resumes the same run.
func (s *Service) Submit(ctx context.Context, p *store.Project, text string) (*store.Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("a message cannot be empty")
	}

	r, err := s.store.ActiveRun(ctx, p.ID)
	if err == nil {
		if r.State != store.RunAwaitingHuman || r.Awaiting() != store.AwaitingQuestion {
			return nil, ErrBusy
		}
		msg, resumed, err := s.store.AppendAnswerAndResume(ctx, p.ID, r.ID, text)
		if err != nil {
			return nil, err
		}
		s.publishMessage(ctx, p.ID, r.ID, store.EventMessageCreated, msg)
		s.publishRun(ctx, resumed)
		s.start(resumed.ID)
		return msg, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	r, msg, err := s.store.CreateRunWithMessage(ctx, p.ID, text)
	if err != nil {
		// run_one_active handles the small race between the read and insert.
		return nil, ErrBusy
	}
	s.publishMessage(ctx, p.ID, r.ID, store.EventMessageCreated, msg)
	s.publishRun(ctx, r)
	s.start(r.ID)
	return msg, nil
}

// Pause is persisted first, then takes effect immediately by cancelling both the
// model stream and any running tool. One press, one meaning.
func (s *Service) Pause(ctx context.Context, projectID string) error {
	r, err := s.store.RequestPause(ctx, projectID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	w := s.workers[r.ID]
	s.mu.Unlock()
	if w != nil {
		w.cancelAll()
	}
	s.publishRun(ctx, r)
	return nil
}

func (s *Service) Resume(ctx context.Context, projectID string) error {
	r, err := s.store.ResumeRun(ctx, projectID)
	if err != nil {
		return err
	}
	s.publishRun(ctx, r)
	s.start(r.ID)
	return nil
}

// Recover picks up runs left mid-flight by a restart. One pass at boot, not a
// ticker: there is one machine, so nothing else can be holding a run.
func (s *Service) Recover(ctx context.Context) error {
	runs, err := s.store.InterruptedRuns(ctx)
	if err != nil {
		return err
	}
	for _, r := range runs {
		s.log.Info("resuming interrupted run", "run_id", r.ID)
		s.start(r.ID)
	}
	return nil
}

func (s *Service) start(runID string) {
	s.mu.Lock()
	if s.workers[runID] != nil {
		s.mu.Unlock()
		return
	}
	w := &worker{}
	s.workers[runID] = w
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.workers, runID)
			s.mu.Unlock()
			s.wg.Done()
		}()
		if err := s.run(runID, w); err != nil {
			s.log.Error("run failed", "run_id", runID, "error", err)
		}
	}()
}

// run is the loop: advance until the model stops asking for tools, the run leaves
// the running state, or the operator pauses.
func (s *Service) run(runID string, w *worker) error {
	ctx := context.Background()
	for {
		r, err := s.store.RunByID(ctx, runID)
		if err != nil {
			return err
		}
		if r.State != store.RunRunning {
			s.publishRun(ctx, r)
			return nil
		}
		if r.PauseRequested {
			return s.pauseRun(runID)
		}
		more, err := s.advance(ctx, r, w)
		if err != nil {
			return s.awaitError(runID, err)
		}
		if !more {
			return nil
		}
	}
}

// advance is exactly one model call plus the tools it asks for.
func (s *Service) advance(ctx context.Context, r *store.Run, w *worker) (bool, error) {
	p, err := s.store.ProjectByID(ctx, r.ProjectID)
	if err != nil {
		return false, err
	}
	cfg, err := s.store.LoadConfig(ctx)
	if err != nil {
		return false, err
	}

	// Close the restart window where a final assistant message landed but the
	// state transition did not.
	if finished, recovered, err := s.store.FinishIfTerminalAssistant(ctx, r.ID); err != nil {
		return false, err
	} else if recovered {
		s.publishRun(ctx, finished)
		s.setState(ctx, r.ProjectID, r.ID, StateIdle, "")
		return false, nil
	}

	// Replay a crash boundary before making another model request. The assistant
	// intent is already in the transcript; only missing results run.
	assistant, pending, err := s.store.TrailingToolCalls(ctx, r.ID)
	if err != nil {
		return false, err
	}
	if len(pending) > 0 {
		if err := s.executeCalls(ctx, r, p, cfg, assistant, pending, w); err != nil {
			return false, err
		}
		return true, nil
	}

	history, err := s.store.ContextMessages(ctx, r.ProjectID)
	if err != nil {
		return false, err
	}
	system, err := s.systemPrompt(ctx, cfg, p)
	if err != nil {
		return false, err
	}
	req := model.Request{
		APIKey: cfg.Secret(store.KeyModelAPIKey), BaseURL: cfg.Text(store.KeyModelBaseURL),
		Model: cfg.Text(store.KeyModelName), System: system, Messages: toModelMessages(history),
		Tools: toolSpecs(s.tools), MaxTokens: cfg.Int("model.max_output_tokens"),
		ReasoningEffort: cfg.Text(store.KeyReasoningEffort),
	}

	s.setState(ctx, r.ProjectID, r.ID, StateThinking, fmt.Sprintf("step %d", r.StepCount+1))
	batch := newDeltaBatch(s.hub, deltaFlush)
	streamCtx, cancel := context.WithCancel(ctx)
	w.setStream(cancel)
	resp, streamErr := s.streamWithRetry(streamCtx, req, model.Handler{
		OnText:     batch.add,
		OnToolCall: func(name string) { s.setState(ctx, r.ProjectID, r.ID, StateWorking, name) },
	})
	cancel()
	w.setStream(nil)
	batch.close()

	if resp == nil {
		if s.pauseRequested(r.ID) {
			return false, s.pauseRun(r.ID)
		}
		return false, fmt.Errorf("model call failed: %w", streamErr)
	}
	if streamErr != nil {
		// Persist whatever arrived so a stream that dies late does not lose work.
		if strings.TrimSpace(resp.Content) != "" {
			msg, appendErr := s.store.AppendMessage(ctx, store.NewMessage{ProjectID: r.ProjectID, RunID: r.ID,
				Role: model.RoleAssistant, Content: resp.Content, TokenCount: resp.Usage.CompletionTokens,
				ReasoningItems: encodeReasoning(resp.Reasoning), Interrupted: true})
			if appendErr != nil {
				return false, appendErr
			}
			s.publishMessage(ctx, r.ProjectID, r.ID, store.EventMessageComplete, msg)
		}
		if s.pauseRequested(r.ID) {
			return false, s.pauseRun(r.ID)
		}
		return false, fmt.Errorf("model stream failed: %w", streamErr)
	}
	if cause := resp.SilentCause(); cause != model.NotSilent {
		return false, silentModelError(cause, resp, cfg.Int("model.max_output_tokens"))
	}

	var encoded json.RawMessage
	if len(resp.ToolCalls) > 0 {
		if encoded, err = json.Marshal(resp.ToolCalls); err != nil {
			return false, err
		}
	}
	assistant, err = s.store.AppendMessage(ctx, store.NewMessage{ProjectID: r.ProjectID, RunID: r.ID,
		Role: model.RoleAssistant, Content: resp.Content, ToolCalls: encoded,
		TokenCount: resp.Usage.CompletionTokens, ReasoningItems: encodeReasoning(resp.Reasoning)})
	if err != nil {
		return false, err
	}
	s.publishMessage(ctx, r.ProjectID, r.ID, store.EventMessageComplete, assistant)
	if _, err := s.store.IncrementRunStep(ctx, r.ID); err != nil {
		return false, err
	}

	// No tool calls means the model has said its piece. The turn is over.
	//
	// Nothing is injected here to push it onwards. A previous version checked
	// whether every phase was complete and appended a synthetic system message
	// telling the model to keep going, which meant the operator could not get a
	// straight answer out of it while a plan was open.
	if len(resp.ToolCalls) == 0 {
		finished, err := s.store.FinishRun(ctx, r.ID, store.RunIdle, "")
		if err != nil {
			return false, err
		}
		s.publishRun(ctx, finished)
		s.setState(ctx, r.ProjectID, r.ID, StateDone, "")
		return false, nil
	}

	ids := make([]string, 0, len(resp.ToolCalls))
	for _, call := range resp.ToolCalls {
		ids = append(ids, call.ID)
	}
	if err := s.executeCalls(ctx, r, p, cfg, assistant, ids, w); err != nil {
		return false, err
	}
	return true, nil
}

// executeCalls runs the requested tools in order, persisting each result.
func (s *Service) executeCalls(ctx context.Context, r *store.Run, p *store.Project, cfg store.AppConfig,
	assistant *store.Message, only []string, w *worker) error {
	var calls []model.ToolCall
	if err := json.Unmarshal(assistant.ToolCalls, &calls); err != nil {
		return fmt.Errorf("decode tool calls: %w", err)
	}
	wanted := make(map[string]bool, len(only))
	for _, id := range only {
		wanted[id] = true
	}
	sb, sbErr := s.projects.ReadySandbox(ctx, p)
	if sbErr != nil {
		return fmt.Errorf("sandbox unavailable before tool execution: %w", sbErr)
	}

	for _, call := range calls {
		if !wanted[call.ID] {
			continue
		}
		if s.pauseRequested(r.ID) {
			return s.pauseRun(r.ID)
		}
		s.setState(ctx, p.ID, r.ID, StateWorking, call.Name)
		s.publish(ctx, p.ID, r.ID, store.EventToolStarted, map[string]any{"tool": call.Name, "call_id": call.ID})

		pad, err := s.store.Scratchpad(ctx, p.ID)
		if err != nil {
			return err
		}
		toolCtx, cancel := context.WithCancel(ctx)
		w.setTool(cancel)
		env := s.env(p, sb, cfg, r.ID, assistant.ID, pad.BaseCommit)
		result, execErr := s.tools.Execute(toolCtx, call.Name, call.Args, env)
		cancel()
		w.setTool(nil)

		if execErr != nil {
			if s.pauseRequested(r.ID) || errors.Is(execErr, context.Canceled) {
				result = tools.Result{Content: "Stopped at the operator's request.", IsError: true}
			} else {
				return fmt.Errorf("tool %s: %w", call.Name, execErr)
			}
		}

		// review is executed by the runner because it needs the model client.
		if !result.IsError && call.Name == "review" {
			request, ok := result.Display.(tools.ReviewRequest)
			if !ok {
				return fmt.Errorf("review returned an invalid request")
			}
			result = s.runReview(ctx, p, cfg, sb, r.ID, assistant.ID, pad, request)
		}

		var display json.RawMessage
		if result.Display != nil {
			display, _ = json.Marshal(result.Display)
		}
		kind := ""
		switch call.Name {
		case "ask_human":
			kind = store.KindAskHuman
		case "review":
			kind = store.KindReview
		}
		in := store.NewMessage{ProjectID: p.ID, RunID: r.ID, Role: model.RoleTool, Kind: kind,
			Content: result.Content, ToolCallID: call.ID, ToolName: call.Name, ToolDisplay: display}

		var msg *store.Message
		var transitioned *store.Run
		if result.IsError {
			// A malformed request is a normal model-correctable tool result, never
			// an infrastructure error that strands the whole run.
			msg, err = s.store.AppendMessage(ctx, in)
		} else {
			msg, transitioned, err = s.applyControl(ctx, p, r, call, result, in, env)
		}
		if err != nil {
			return err
		}
		s.publishMessage(ctx, p.ID, r.ID, store.EventToolComplete, msg)
		if transitioned != nil {
			s.publishRun(ctx, transitioned)
			return nil
		}
		current, err := s.store.RunByID(ctx, r.ID)
		if err != nil {
			return err
		}
		if current.State != store.RunRunning {
			s.publishRun(ctx, current)
			return nil
		}
	}
	return nil
}

// applyControl persists the durable effect of a control tool alongside its result.
// Ordinary tools just get their result appended.
func (s *Service) applyControl(ctx context.Context, p *store.Project, r *store.Run, call model.ToolCall,
	result tools.Result, in store.NewMessage, env *tools.Env) (*store.Message, *store.Run, error) {
	switch call.Name {
	case "ask_human":
		payload := map[string]any{"question": result.Content}
		if result.Display != nil {
			payload["question"] = result.Display
		}
		return s.store.AppendToolResultAndAwait(ctx, in, store.AwaitingQuestion, payload)

	case "create_plan":
		request, ok := result.Display.(tools.PlanRequest)
		if !ok {
			return nil, nil, fmt.Errorf("create_plan returned an invalid plan")
		}
		draft := store.PlanDraft{Title: request.Title, Tasks: request.Tasks, Spec: store.Spec{
			Problem: request.Problem, Approach: request.Approach, Verification: request.Verification,
			EdgeCases: request.EdgeCases, Risks: request.Risks, Questions: request.Questions,
		}}
		pad, awaiting, msg, err := s.store.WritePlan(ctx, p.ID, r.ID, draft, in)
		if err != nil {
			return nil, nil, err
		}
		s.publish(ctx, p.ID, r.ID, store.EventPlanUpdated, map[string]any{"title": pad.Title, "tasks": len(pad.Tasks), "status": pad.Status})
		return msg, awaiting, nil

	case "update_task":
		request, ok := result.Display.(tools.TaskUpdate)
		if !ok {
			return nil, nil, fmt.Errorf("update_task returned an invalid update")
		}
		pad, msg, err := s.store.UpdateTask(ctx, p.ID, request.Task, request.Status, request.Note, in)
		if err != nil {
			// A bad task number is the model's mistake to correct, not a crash.
			msg, appendErr := s.store.AppendMessage(ctx, failed(in, err))
			return msg, nil, appendErr
		}
		s.publish(ctx, p.ID, r.ID, store.EventPlanUpdated, map[string]any{"title": pad.Title, "tasks": len(pad.Tasks), "status": pad.Status})
		return msg, nil, nil

	case "remember":
		request, ok := result.Display.(tools.MemoriesUpdate)
		if !ok {
			return nil, nil, fmt.Errorf("remember returned an invalid update")
		}
		if err := s.store.SetMemories(ctx, p.ID, request.Memories); err != nil {
			return nil, nil, err
		}
		msg, err := s.store.AppendMessage(ctx, in)
		return msg, nil, err

	case "record_orientation":
		request, ok := result.Display.(tools.OrientationUpdate)
		if !ok {
			return nil, nil, fmt.Errorf("record_orientation returned an invalid update")
		}
		if err := s.store.SetOrientation(ctx, p.ID, request.Orientation); err != nil {
			return nil, nil, err
		}
		msg, err := s.store.AppendMessage(ctx, in)
		return msg, nil, err

	case "done":
		request, ok := result.Display.(tools.DoneRequest)
		if !ok {
			return nil, nil, fmt.Errorf("done returned an invalid request")
		}
		return s.ship(ctx, p, r, call.ID, request, env)

	default:
		msg, err := s.store.AppendMessage(ctx, in)
		return msg, nil, err
	}
}

// failed rewrites a tool result as a model-visible failure.
func failed(in store.NewMessage, err error) store.NewMessage {
	in.Content = "That did not work: " + err.Error()
	in.ToolDisplay = nil
	return in
}

// ship pushes the branch, opens or updates the pull request, and ends the run.
//
// A push failure parks the run for the operator, because there is nothing useful
// the model can do about a rejected credential. A pull request failure does not:
// the commits are already on the remote.
func (s *Service) ship(ctx context.Context, p *store.Project, r *store.Run, callID string,
	request tools.DoneRequest, env *tools.Env) (*store.Message, *store.Run, error) {
	in := func(content string, display any) store.NewMessage {
		encoded, _ := json.Marshal(display)
		return store.NewMessage{ProjectID: p.ID, RunID: r.ID, Role: model.RoleTool, Content: content,
			ToolCallID: callID, ToolName: "done", ToolDisplay: encoded}
	}

	clean, err := env.Sandbox.Exec(ctx, sandbox.Command{Cmd: "git status --porcelain", Dir: project.RepoPath, Timeout: time.Minute})
	if err != nil {
		return nil, nil, fmt.Errorf("check worktree before shipping: %w", err)
	}
	if strings.TrimSpace(clean.Output()) != "" {
		msg, appendErr := s.store.AppendMessage(ctx, in(
			"done needs a clean worktree. Commit or discard the remaining changes, then call done again.",
			map[string]any{"summary": request.Summary}))
		return msg, nil, appendErr
	}

	// The work has to have been reviewed, and the review has to have reached a
	// verdict.
	//
	// This used to check only that a review had been attempted, which was safe while
	// attempting one implied getting one. It did not: the reviewer's budget was five
	// model calls and running out was its ordinary outcome, so a run could ship with
	// a review in the log and nobody having formed an opinion. The reviewer can
	// finish now, so this can ask for what it actually wants.
	//
	// It still cannot trap the run. A reviewer that is genuinely broken — no diff, a
	// dead stream — fails the same way every time, so after two attempts the agent is
	// allowed through on the condition that it says the review was inconclusive.
	// Blocking forever on a component the operator cannot repair from here would be
	// worse than shipping with a stated caveat.
	attempts, concluded, err := s.store.ReviewOutcomes(ctx, r.ID)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case attempts == 0:
		msg, appendErr := s.store.AppendMessage(ctx, in(
			"Before shipping, call review so an independent reviewer reads the diff. "+
				"Deal with anything real it finds, then call done again.",
			map[string]any{"summary": request.Summary}))
		return msg, nil, appendErr
	case !concluded && attempts < 2:
		msg, appendErr := s.store.AppendMessage(ctx, in(
			"The review did not reach a verdict, so nothing has actually been reviewed yet. "+
				"Call review again. If it fails the same way a second time, say so in your summary "+
				"and call done — but do not describe this work as reviewed.",
			map[string]any{"summary": request.Summary}))
		return msg, nil, appendErr
	}

	pushArgs, _ := json.Marshal(map[string]bool{"force_with_lease": false})
	push, err := s.shipping.Execute(ctx, "git_push", pushArgs, env)
	if err != nil {
		return nil, nil, fmt.Errorf("push for done: %w", err)
	}
	if push.IsError {
		content := "Shipping stopped: pushing " + store.WorkBranch + " failed. " + push.Content
		return s.store.AppendToolResultAndAwait(ctx,
			in(content, map[string]any{"summary": request.Summary, "push": push.Display}),
			store.AwaitingError,
			map[string]string{"error": push.Content, "action": "Fix the push problem, then press Resume."})
	}

	title := request.Summary
	if pad, padErr := s.store.Scratchpad(ctx, p.ID); padErr == nil && pad.Title != "" {
		title = pad.Title
	}
	prArgs, _ := json.Marshal(map[string]string{"title": title, "body": request.Summary})
	pr, err := s.shipping.Execute(ctx, "create_pr", prArgs, env)
	if err != nil {
		pr = tools.Result{Content: "Could not open a pull request after the successful push: " + err.Error(), IsError: true}
	}

	content := fmt.Sprintf("Shipped `%s`.\n\n%s\n\nPreview: /", store.WorkBranch, request.Summary)
	if pr.IsError {
		content += "\n\nNo pull request was opened, but the push succeeded: " + pr.Content
	} else {
		content += "\n\n" + pr.Content
	}

	if err := s.store.ClearPlan(ctx, p.ID); err != nil {
		return nil, nil, err
	}
	// shipped marks this as the one done result that actually shipped. The three
	// earlier returns above also produce a done-named tool message — a dirty
	// worktree, an unreviewed diff, a failed push — and without an explicit flag the
	// UI could only tell them apart by guessing at which keys happened to be
	// present, which it got wrong and reported all four as a success.
	msg, err := s.store.AppendMessage(ctx, in(content, map[string]any{
		"summary": request.Summary, "shipped": true, "preview_url": "/",
		"push": push.Display, "pull_request": pr.Display, "pr_error": pr.IsError}))
	if err != nil {
		return nil, nil, err
	}
	finished, err := s.store.FinishRun(ctx, r.ID, store.RunDone, "")
	if err != nil {
		return nil, nil, err
	}
	// Every other terminal path announces itself; this one did not. Without it the
	// spinner kept reading "working · done" until a reload arrived, and that reload
	// is deferred for as long as the composer holds text or focus.
	s.setState(ctx, r.ProjectID, r.ID, StateDone, "shipped")
	return msg, finished, nil
}

// streamWithRetry retries only calls that produce no model output. Once text or a
// tool call appears, replaying the request could duplicate a durable boundary and
// is therefore forbidden. The reviewer shares this policy.
func (s *Service) streamWithRetry(ctx context.Context, req model.Request, handler model.Handler) (*model.Response, error) {
	var last error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resp, err := s.model.Stream(ctx, req, handler)
		if err == nil || errors.Is(err, context.Canceled) {
			return resp, err
		}
		if resp != nil && (strings.TrimSpace(resp.Content) != "" || len(resp.ToolCalls) > 0) {
			return resp, err
		}
		last = err
		if attempt == maxAttempts-1 {
			break
		}
		delay := time.Duration(1<<attempt) * time.Second
		if delay > maxBackoff {
			delay = maxBackoff
		}
		jitter := time.Duration(rand.Int63n(int64(delay)/2 + 1))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay/2 + jitter):
		}
	}
	return nil, last
}

func (s *Service) env(p *store.Project, sb sandbox.Sandbox, cfg store.AppConfig,
	runID string, messageID int64, baseCommit string) *tools.Env {
	return &tools.Env{
		Project: p, Sandbox: sb, Store: s.store, Log: s.log,
		RunID: runID, MessageID: messageID, BaseCommit: baseCommit,
		// Read from the snapshot the step is already holding, so a token corrected in
		// Settings applies to the next tool call rather than the next deploy.
		GitHubToken: cfg.Secret(store.KeyGitHubToken),
		Emit:        func(ev tools.Event) { s.publishLive(ev.Type, ev) },
	}
}

func (s *Service) pauseRun(runID string) error {
	r, err := s.store.MarkPaused(context.Background(), runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	s.publishRun(context.Background(), r)
	s.setState(context.Background(), r.ProjectID, r.ID, StateIdle, "paused")
	return nil
}

// silentModelError explains a response that carried nothing to act on.
//
// One sentence per cause, and only the two budget causes mention the budget. The
// version this replaced said "exhausted its output budget on reasoning" for every
// empty response including the ones that never got as far as reasoning, and then
// told the operator to raise a limit — which, past the API ceiling, is how a
// recoverable blip became a permanent 400 on every subsequent request.
func silentModelError(cause model.SilentCause, resp *model.Response, budget int) error {
	switch cause {
	case model.SilentTruncated:
		return fmt.Errorf("the model reached its output limit of %d tokens without producing anything to act on. "+
			"Raise Max output tokens in Settings, up to %d", budget, model.MaxOutputTokens)
	case model.SilentReasoning:
		return fmt.Errorf("the model spent %d output tokens reasoning and returned no text and no tool calls. "+
			"Raise Max output tokens in Settings, up to %d", resp.Usage.CompletionTokens, model.MaxOutputTokens)
	default:
		return fmt.Errorf("the model returned no text, no tool calls, and no token usage, and gave no reason. " +
			"That is a dropped or rejected request rather than a budget problem: check the model name and " +
			"credentials in Settings, then press Resume")
	}
}

// awaitError parks the run for the operator with an actionable next step.
func (s *Service) awaitError(runID string, cause error) error {
	r, err := s.store.RunByID(context.Background(), runID)
	if err != nil {
		return err
	}
	action := "Read the message below, fix the underlying problem, then press Resume."
	lower := strings.ToLower(cause.Error())
	switch {
	case errors.Is(cause, sandbox.ErrNotFound):
		if err := s.store.SetSandboxStatus(context.Background(), r.ProjectID, store.SandboxMissing); err != nil {
			s.log.Error("mark sandbox missing", "project_id", r.ProjectID, "error", err)
		}
		action = "The sandbox is gone. Open Project, recreate it (uncommitted work is lost), wait for setup, then press Resume."
	case strings.Contains(lower, "model") || strings.Contains(lower, "stream"):
		action = "The model request was retried and still failed. Check Settings, then press Resume."
	}
	awaiting, err := s.store.AwaitHuman(context.Background(), runID, store.AwaitingError,
		map[string]string{"error": cause.Error(), "action": action}, cause.Error())
	if err != nil {
		return err
	}
	s.publishRun(context.Background(), awaiting)
	s.setState(context.Background(), r.ProjectID, runID, StateError, cause.Error())
	return nil
}

func (s *Service) pauseRequested(runID string) bool {
	r, err := s.store.RunByID(context.Background(), runID)
	return err == nil && r.PauseRequested
}

// Shutdown stops the in-flight run at its next check and waits briefly.
//
// Nothing is drained to a boundary and no lease is handed over: a run left in the
// running state is picked up by Recover when the process comes back, and the
// trailing tool-call repair makes that safe.
func (s *Service) Shutdown(ctx context.Context) {
	s.mu.Lock()
	for _, w := range s.workers {
		w.cancelAll()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.log.Warn("runner did not stop within the shutdown grace period")
	}
}

// ApprovePlan releases the board for work and starts the same run.
func (s *Service) ApprovePlan(ctx context.Context, p *store.Project) error {
	base := ""
	if sb, err := s.projects.ReadySandbox(ctx, p); err == nil {
		if head, execErr := sb.Exec(ctx, sandbox.Command{Cmd: "git rev-parse HEAD", Dir: project.RepoPath, Timeout: time.Minute}); execErr == nil && head.ExitCode == 0 {
			base = strings.TrimSpace(head.Stdout)
		}
	}
	pad, r, message, err := s.store.ApprovePlan(ctx, p.ID, base)
	if err != nil {
		return err
	}
	s.publishMessage(ctx, p.ID, r.ID, store.EventMessageCreated, message)
	s.publish(ctx, p.ID, r.ID, store.EventPlanUpdated, map[string]any{"title": pad.Title, "tasks": len(pad.Tasks), "status": pad.Status})
	s.publishRun(ctx, r)
	s.start(r.ID)
	return nil
}

// RevisePlan returns the board to the model with feedback.
func (s *Service) RevisePlan(ctx context.Context, projectID, feedback string) error {
	r, message, err := s.store.RevisePlan(ctx, projectID, feedback)
	if err != nil {
		return err
	}
	s.publishMessage(ctx, projectID, r.ID, store.EventMessageCreated, message)
	s.publish(ctx, projectID, r.ID, store.EventPlanUpdated, map[string]any{"status": "revising"})
	s.publishRun(ctx, r)
	s.start(r.ID)
	return nil
}

func (s *Service) setState(ctx context.Context, projectID, runID, state, detail string) {
	s.publish(ctx, projectID, runID, store.EventAgentState, map[string]any{"state": state, "detail": detail})
}
func (s *Service) publishRun(ctx context.Context, r *store.Run) {
	s.publish(ctx, r.ProjectID, r.ID, store.EventRunState, map[string]any{
		"run_id": r.ID, "state": r.State, "reason": r.Awaiting(), "step": r.StepCount, "pause_requested": r.PauseRequested})
}
func (s *Service) publishMessage(ctx context.Context, projectID, runID, eventType string, m *store.Message) {
	s.publish(ctx, projectID, runID, eventType, map[string]any{
		"message_id": m.ID, "role": m.Role, "kind": m.MessageKind(), "tool": m.Tool()})
}
func (s *Service) publish(ctx context.Context, projectID, runID, eventType string, payload any) {
	writeCtx := ctx
	if writeCtx.Err() != nil {
		var cancel context.CancelFunc
		writeCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
	}
	ev, err := s.store.AppendEvent(writeCtx, projectID, runID, eventType, payload)
	if err != nil {
		s.log.Error("append event", "error", err, "type", eventType)
		return
	}
	s.hub.Publish(events.Frame{ID: ev.ID, Type: ev.Type, Data: ev.Payload})
}
func (s *Service) publishLive(eventType string, payload any) {
	if data, err := json.Marshal(payload); err == nil {
		s.hub.Publish(events.Frame{Type: eventType, Data: data})
	}
}

// encodeReasoning prepares reasoning items for storage.
//
// nil for a turn that produced none, so the column stays NULL rather than holding an
// empty array — the difference between "this model does not return reasoning" and
// "this turn happened not to have any" is worth being able to see in the database.
func encodeReasoning(items []model.ReasoningItem) json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return encoded
}

// toModelMessages rebuilds the model's view of the conversation from the transcript.
//
// The reasoning items go back in here. That is the entire payoff of the Responses
// API migration: without this the model re-derives its chain of thought on every
// turn of a tool loop, spending the output budget on work it already did and losing
// the thread of anything that takes more than one step.
func toModelMessages(rows []*store.Message) []model.Message {
	out := make([]model.Message, 0, len(rows))
	for _, m := range rows {
		msg := model.Message{Role: m.Role, Content: m.Content, ToolCallID: m.CallID()}
		if m.HasToolCalls() {
			_ = json.Unmarshal(m.ToolCalls, &msg.ToolCalls)
		}
		if m.HasReasoning() {
			// A decode failure drops the reasoning rather than failing the turn: losing
			// continuity costs quality, and refusing to build the request costs the run.
			_ = json.Unmarshal(m.ReasoningItems, &msg.Reasoning)
		}
		out = append(out, msg)
	}
	return out
}

func toolSpecs(r *tools.Registry) []model.ToolSpec {
	specs := r.Specs()
	out := make([]model.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if encoded, err := json.Marshal(spec.Params); err == nil {
			out = append(out, model.ToolSpec{Name: spec.Name, Description: spec.Description, Parameters: encoded})
		}
	}
	return out
}

// deltaBatch coalesces streamed text into periodic SSE frames, so a fast stream
// does not become thousands of tiny writes to the browser.
type deltaBatch struct {
	hub    *events.Hub
	every  time.Duration
	mu     sync.Mutex
	buf    strings.Builder
	timer  *time.Timer
	closed bool
}

func newDeltaBatch(hub *events.Hub, every time.Duration) *deltaBatch {
	return &deltaBatch{hub: hub, every: every}
}
func (b *deltaBatch) add(delta string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.buf.WriteString(delta)
	if b.timer == nil {
		b.timer = time.AfterFunc(b.every, b.flush)
	}
}
func (b *deltaBatch) flush() {
	b.mu.Lock()
	text := b.buf.String()
	b.buf.Reset()
	b.timer = nil
	b.mu.Unlock()
	if text == "" {
		return
	}
	if data, err := json.Marshal(map[string]string{"text": text}); err == nil {
		b.hub.Publish(events.Frame{Type: "message.delta", Data: data})
	}
}
func (b *deltaBatch) close() {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.closed = true
	b.mu.Unlock()
	b.flush()
}
