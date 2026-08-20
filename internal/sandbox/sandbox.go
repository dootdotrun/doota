// Package sandbox is the agent's execution environment.
//
// One sandbox per project, persistent for the project's life. Fly Sprites is the
// provider; the interface exists so the agent loop and tool layer never import
// an SDK type, and so the whole stack can be exercised without a live account.
package sandbox

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"sort"
	"strings"
	"time"
)

// State is the lifecycle state of a sandbox.
type State string

const (
	StateUnknown  State = "unknown"
	StateRunning  State = "running"
	StateSleeping State = "sleeping"
	StateMissing  State = "missing"
)

// Provider creates and destroys sandboxes.
type Provider interface {
	// Kind identifies the implementation, for logs and the Project screen.
	Kind() string
	Create(ctx context.Context, name string) (Sandbox, error)
	Get(ctx context.Context, name string) (Sandbox, error)
	Delete(ctx context.Context, name string) error
	Close() error
}

// Sandbox is one persistent Linux environment.
type Sandbox interface {
	Name() string
	State(ctx context.Context) (State, error)

	// Wake starts a sleeping sandbox. Safe to call when already running.
	Wake(ctx context.Context) error

	// URL is the sandbox's own public address, if the provider has one. It is
	// informational: previews are served through the app's proxy instead, so
	// they inherit the app's session.
	URL(ctx context.Context) (string, error)

	// Path translates a sandbox-absolute path into one the sandbox's own shell
	// will resolve.
	//
	// Command.Dir and the file methods already take sandbox-absolute paths and
	// translate them internally. Path is for the case they cannot help with: a
	// path embedded inside a Command.Cmd string, which the provider has no way
	// to find and rewrite. For the real provider this is the identity function;
	// for the local one it maps onto the backing directory.
	Path(sandboxPath string) string

	Exec(ctx context.Context, cmd Command) (ExecResult, error)
	ExecStream(ctx context.Context, cmd Command, stdout, stderr io.Writer) (int, error)

	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error

	// Checkpoints are deliberately absent. Both providers implemented list, create
	// and restore, the Project screen offered a restore button, and nothing in the
	// codebase ever called create — so the list was always empty and the button
	// unreachable. Restoring the filesystem while the operator has a branch pushed
	// is also a confusing recovery story next to "recreate the sandbox", which
	// exists and works.

	// DialPort opens a connection to a TCP port inside the sandbox. This is how
	// the preview proxy reaches a dev server on any port.
	DialPort(ctx context.Context, port int) (net.Conn, error)
}

// Command is a shell command line to run inside the sandbox.
//
// A shell line rather than an argv slice: it is what the agent's bash tool will
// pass through in Phase 3, and it keeps pipelines and redirection working
// without a special case.
type Command struct {
	Cmd     string
	Dir     string
	Env     map[string]string
	Timeout time.Duration
}

// ExecResult is the outcome of a command.
//
// Stderr may be empty even when the command wrote to it: the Sprites transport
// delivers one merged stream, so that provider returns everything in Stdout.
// Read Output() rather than either field when you want what the command printed.
//
// Not worth papering over by re-running commands or wrapping every line in
// markers. A terminal shows one interleaved stream too, and the agent is reading
// this the way a person reads a terminal. What would be unacceptable is losing
// the bytes, and they are not lost.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Output is everything the command printed, whether or not the provider could
// tell the two streams apart.
func (r ExecResult) Output() string {
	if r.Stderr == "" {
		return r.Stdout
	}
	if r.Stdout == "" {
		return r.Stderr
	}
	return r.Stdout + "\n" + r.Stderr
}

// There was a Combined() next to Output() with a byte-identical implementation and
// a comment about what a human reading a setup log wants. Two names for one
// function is two things to keep in step.

// ErrNotFound means the sandbox does not exist.
var ErrNotFound = fmt.Errorf("sandbox not found")

// DefaultTimeout bounds a command that did not ask for one.
const DefaultTimeout = 2 * time.Minute

// shellLine wraps a command so environment variables are exported rather than
// replacing the environment wholesale.
//
// Both SDK and os/exec take a full replacement environment, and replacing it
// would drop PATH and everything else the image sets up. Exporting inside the
// shell adds without destroying.
func shellLine(cmd Command) string {
	if len(cmd.Env) == 0 {
		return cmd.Cmd
	}
	keys := make([]string, 0, len(cmd.Env))
	for k := range cmd.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic, so logs and tests are stable

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s; ", k, shellQuote(cmd.Env[k]))
	}
	b.WriteString(cmd.Cmd)
	return b.String()
}

// shellQuote single-quotes a value for safe interpolation into a shell line.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func timeoutOf(cmd Command) time.Duration {
	if cmd.Timeout > 0 {
		return cmd.Timeout
	}
	return DefaultTimeout
}
