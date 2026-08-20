// Package tools is everything the agent can do.
//
// Two registries: the primary agent gets every tool, the reviewer subagent gets
// only the read-only ones. The reviewer's inability to write is a property of the
// registry it is constructed with, not an instruction in its prompt — a prompt
// restriction is a suggestion, an absent tool is a guarantee.
//
// Nothing here knows about the model. A tool takes JSON in and returns text out,
// so the whole layer is exercisable without an API key, which is the entire point
// of building it before the agent loop: a failing goal is never ambiguous between
// "the model is confused" and "the tool is broken".
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/sandbox"
	"github.com/dootdotrun/doot-ai/internal/store"
)

// Tool is one capability the agent can invoke.
type Tool interface {
	Name() string

	// Spec is the JSON schema advertised to the model.
	Spec() ToolSpec

	// Execute runs the tool. See Result for the error split.
	Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error)
}

// ToolSpec describes a tool to the model.
//
// Deliberately not the OpenAI SDK's type. The model client adapter translates
// this in Phase 4, which keeps the SDK out of every tool file and makes the specs
// independently inspectable without a dependency on the wire format.
type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Params      Params `json:"parameters"`
}

// Params is a JSON Schema object describing a tool's arguments.
type Params struct {
	Type       string           `json:"type"`
	Properties map[string]Param `json:"properties"`
	Required   []string         `json:"required,omitempty"`
}

// Param is one argument in a tool's schema.
type Param struct {
	Type        string           `json:"type"`
	Description string           `json:"description,omitempty"`
	Enum        []string         `json:"enum,omitempty"`
	Items       *Param           `json:"items,omitempty"`
	Properties  map[string]Param `json:"properties,omitempty"`
	Required    []string         `json:"required,omitempty"`
}

// object builds a parameter schema. Required keys are listed explicitly rather
// than inferred, so a schema reads the same way the model receives it.
func object(props map[string]Param, required ...string) Params {
	return Params{Type: "object", Properties: props, Required: required}
}

// Env is everything a tool executes against.
//
// Sandbox is nil for tools that do not touch it, and Emit is nil in background
// setup or recovery boundaries; every tool must tolerate both without assuming it
// is always mid-conversation.
type Env struct {
	Project *store.Project
	Sandbox sandbox.Sandbox
	Store   *store.Store
	Config  store.AppConfig
	Log     *slog.Logger

	// RunID is "" for a call made outside a run. The durable run type arrives in
	// Phase 5; until then the id is all the tool layer needs, and it is what
	// tool_call_log records.
	RunID string

	// MessageID is the assistant message that requested the call, or 0 when there
	// is none.
	MessageID int64

	// BaseCommit is the commit the current plan was approved at. git_diff defaults
	// its "from" to this, so the reviewer sees exactly this plan's work without
	// being told a SHA. Empty when there is no plan, and git_diff falls back to
	// the working tree.
	BaseCommit string

	// GitHubToken authenticates pull request creation against GitHub's REST API.
	// The same PAT
	// is installed in the sandbox during provisioning for git clone, fetch, and
	// push, so there is one credential behind every GitHub operation.
	//
	// Passed through Env rather than read from Config because it is a secret, and
	// app_config is rendered on the Settings screen.
	GitHubToken string

	// Emit streams progress to the UI mid-execution. Nil when nothing is
	// listening. Use Env.emit rather than calling this directly.
	Emit func(Event)
}

// Event is a UI progress notification emitted while a tool runs.
//
// The SSE hub that carries these is built in Phase 4; the shape is fixed now so
// tools do not need revisiting then.
type Event struct {
	Type    string `json:"type"`
	Tool    string `json:"tool,omitempty"`
	Payload any    `json:"payload,omitempty"`
}

// Event types emitted by tools.
const (
	EventToolOutput = "tool_output" // incremental command output
	EventToolDiff   = "tool_diff"   // a file changed
)

// emit sends an event if anything is listening.
func (e *Env) emit(ev Event) {
	if e == nil || e.Emit == nil {
		return
	}
	e.Emit(ev)
}

// logger returns a usable logger even when the caller supplied none.
func (e *Env) logger() *slog.Logger {
	if e == nil || e.Log == nil {
		return slog.Default()
	}
	return e.Log
}

// needSandbox returns the sandbox, or an infrastructure error.
//
// A missing sandbox is not something the agent can reason its way out of, so this
// returns a Go error and escalates the run rather than reporting a tool failure
// the model would probably retry.
func (e *Env) needSandbox() (sandbox.Sandbox, error) {
	if e == nil || e.Sandbox == nil {
		return nil, fmt.Errorf("no sandbox available")
	}
	return e.Sandbox, nil
}

// needProject returns the project, or an infrastructure error.
func (e *Env) needProject() (*store.Project, error) {
	if e == nil || e.Project == nil {
		return nil, fmt.Errorf("no project available")
	}
	return e.Project, nil
}

// Result is what a tool returns.
//
// Content and Display are separate on purpose. The model wants terse text; the
// phone wants a rendered diff or a findings list. Forcing one representation to
// serve both means either bloating the context with markup or showing the human
// raw tool output.
//
// IsError is a tool-level failure: a command exited non-zero, a file was not
// found, an edit matched nothing. It goes back to the model as an ordinary tool
// result so the agent can react and correct itself. Only a returned Go error
// means the infrastructure is broken, and that escalates the run to
// awaiting_human(error). This split is what keeps the agent self-correcting
// instead of stopping to ask every time it mistypes a path.
type Result struct {
	Content string `json:"content"`
	Display any    `json:"display,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// ok builds a successful result.
func ok(format string, args ...any) Result {
	return Result{Content: fmt.Sprintf(format, args...)}
}

// fail builds a tool-level failure, which the model sees and can act on.
//
// Messages are written for a reader who has to decide what to do next, so they
// say what went wrong and what would work instead.
func fail(format string, args ...any) Result {
	return Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// decode unmarshals tool arguments.
//
// Malformed arguments are the model's mistake, not a broken tool, so callers turn
// the returned error into a Result with IsError set rather than escalating.
func decode(in json.RawMessage, dst any) error {
	if len(strings.TrimSpace(string(in))) == 0 {
		in = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("could not parse arguments: %w", err)
	}
	return nil
}

// shellQuote single-quotes a value for safe interpolation into a shell line.
//
// Duplicated from internal/sandbox rather than exported from it: three lines of
// obvious code is a smaller cost than widening that package's API for every
// caller that builds a command string.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// There is deliberately no guard here against the agent pushing from bash.
//
// The previous version substring-matched raw shell commands for "git" plus "push"
// to stop the agent shipping outside done. It was bypassable by anyone who wanted
// to (`git  push`, a script file, a variable) while blocking things nobody meant to
// block, like `git log --grep pushdown`. A guard that stops no determined caller
// and does stop honest ones is worse than none: if the agent pushes early, the
// operator sees the branch move, which is the same feedback the guard gave.
