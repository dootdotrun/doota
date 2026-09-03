package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/dootdotrun/doot-ai/internal/store"
)

// Registry is a fixed set of tools.
//
// There is no Add method and no mutation after construction. A registry that can
// be extended at runtime is a registry whose contents you have to go and check;
// these two are declared in one place below and are what they say they are.
type Registry struct {
	order  []string
	byName map[string]Tool
}

// NewRegistry builds a registry. Duplicate names panic: it is a programming error
// discoverable at boot, and silently keeping one of the two would be worse.
func NewRegistry(list ...Tool) *Registry {
	r := &Registry{
		order:  make([]string, 0, len(list)),
		byName: make(map[string]Tool, len(list)),
	}
	for _, t := range list {
		name := t.Name()
		if _, dup := r.byName[name]; dup {
			panic(fmt.Sprintf("tools: duplicate tool %q", name))
		}
		r.byName[name] = t
		r.order = append(r.order, name)
	}
	sort.Strings(r.order)
	return r
}

// Get looks a tool up by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, found := r.byName[name]
	return t, found
}

// Names lists every tool, sorted.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Len is how many tools the registry holds.
func (r *Registry) Len() int { return len(r.order) }

// Specs returns every tool's schema, for the model request.
func (r *Registry) Specs() []ToolSpec {
	out := make([]ToolSpec, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.byName[name].Spec())
	}
	return out
}

// readOnly is the reviewer's entire world: five tools, none of which can change
// anything. It is also why the reviewer cannot "helpfully" fix what it finds,
// which would make its findings unreviewable.
func readOnly() []Tool {
	return []Tool{
		readFileTool{},
		listDirTool{},
		searchTool{},
		readLogsTool{},
		gitDiffTool{},
	}
}

// writeTools mutate the sandbox or the working tree.
//
// git_push and create_pr are deliberately absent. Shipping happens through done,
// which pushes and opens the pull request itself. When they were model-facing tools
// they needed a guard to stop the agent shipping early, that guard needed a
// Shipping flag on Env to let done bypass it, and the flag needed a substring
// matcher over raw shell commands to stop bash going around it. Removing the tools
// removed all three.
func writeTools() []Tool {
	return []Tool{
		writeFileTool{},
		editFileTool{},
		bashTool{},
		bashBgTool{},
		stopBgTool{},
		httpCheckTool{},
		exposePortTool{},
		gitCommitTool{},
	}
}

// controlTools drive the loop itself rather than the sandbox.
//
// record_orientation is here rather than among the write tools because what it
// changes is the agent's own durable context, not the sandbox.
func controlTools() []Tool {
	return []Tool{
		createPlanTool{},
		updateTaskTool{},
		reviewTool{},
		uiReviewTool{},
		askHumanTool{},
		rememberTool{},
		orientTool{},
		doneTool{},
	}
}

// Primary is the registry the main agent gets.
func Primary() *Registry {
	all := append(readOnly(), writeTools()...)
	all = append(all, controlTools()...)
	return NewRegistry(all...)
}

// Reviewer is the registry the reviewer subagent gets: read-only, no exceptions.
func Reviewer() *Registry {
	return NewRegistry(readOnly()...)
}

// UIReviewer is the registry the UI subagent gets.
//
// Read-only plus a camera. It can look at the code and it can look at the rendered
// page, and it cannot change either — the same reason the semantic reviewer cannot
// fix what it finds: a reviewer that edits the thing it is reviewing produces
// findings nobody can check.
//
// screenshot is deliberately not in Primary. A tool result on this transport is a
// string, so the primary agent could take a picture and would then have no way to see
// it; the only honest use of a camera is by an agent that builds its own requests.
func UIReviewer() *Registry {
	return NewRegistry(append(readOnly(), screenshotTool{})...)
}

// Shipping holds the tools the runner may call but the model may not.
//
// They go through a registry rather than being called directly so a push and a
// pull request attempt still land in tool_call_log, which is where you look when a
// ship did not do what you expected. Keeping them out of Primary is what stops the
// model shipping whenever it feels finished.
func Shipping() *Registry {
	return NewRegistry(gitPushTool{}, createPRTool{})
}

// Execute runs a tool by name, records it, and returns its result.
//
// Every execution writes a tool_call_log row — successes, tool-level failures,
// unparseable arguments, and calls for tools that do not exist. That completeness
// is the point: a run where the model repeatedly called a misremembered tool name
// is diagnosable afterwards only if the attempts were recorded.
//
// There is no phase gate. A previous version refused write_file, edit_file, bash
// and git_commit unless the current phase was durably in_progress, which meant the
// agent could be told its own file edit was illegal because a bookkeeping row was
// in the wrong state.
//
// The returned error is reserved for broken infrastructure. Anything the agent
// could plausibly recover from comes back as a Result with IsError set.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage, env *Env) (Result, error) {
	started := time.Now()

	tool, found := r.Get(name)
	if !found {
		res := fail("no tool named %q. Available: %v", name, r.Names())
		r.record(ctx, env, name, args, res, time.Since(started))
		return res, nil
	}

	res, err := tool.Execute(ctx, args, env)
	elapsed := time.Since(started)

	if err != nil {
		// Infrastructure failure. Logged as an error row so the forensic record
		// shows the attempt, then escalated to the caller.
		r.record(ctx, env, name, args, Result{Content: err.Error(), IsError: true}, elapsed)
		return Result{}, err
	}

	r.record(ctx, env, name, args, res, elapsed)
	env.logger().Info("tool call",
		"tool", name,
		"duration_ms", elapsed.Milliseconds(),
		"is_error", res.IsError,
	)
	return res, nil
}

// record writes the tool_call_log row.
//
// A failure to log is logged and swallowed. Losing the forensic record of a call
// is bad; failing a tool that already did its work because the audit write failed
// afterwards is worse, and would leave the model with no idea whether the file was
// written.
func (r *Registry) record(ctx context.Context, env *Env, name string, args json.RawMessage, res Result, elapsed time.Duration) {
	if env == nil || env.Store == nil {
		return
	}

	// The caller's context may already be cancelled — a paused run, a closed
	// request — and the row still has to land.
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if _, err := env.Store.LogToolCall(logCtx, store.ToolCallRecord{
		RunID:     env.RunID,
		MessageID: env.MessageID,
		ToolName:  name,
		Args:      args,
		Content:   res.Content,
		Display:   res.Display,
		IsError:   res.IsError,
		Duration:  elapsed,
	}); err != nil {
		env.logger().Error("log tool call", "error", err, "tool", name)
	}
}
