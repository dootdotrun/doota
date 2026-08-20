package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dootdotrun/doot-ai/internal/project"
	"github.com/dootdotrun/doot-ai/internal/sandbox"
	"github.com/dootdotrun/doot-ai/internal/store"
)

// ---------------------------------------------------------------------------
// bash_bg
// ---------------------------------------------------------------------------

type bashBgTool struct{}

type bashBgArgs struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
}

func (bashBgTool) Name() string { return "bash_bg" }

func (bashBgTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "bash_bg",
		Description: "Start a long-running process — a dev server, a watcher — detached, with its output " +
			"captured to a log file you can read with read_logs. Starting a process with an existing " +
			"name replaces it.",
		Params: object(map[string]Param{
			"name":    {Type: "string", Description: `Short identifier used to read logs and stop it later, for example "dev".`},
			"command": {Type: "string", Description: "Shell command line to run."},
			"cwd":     {Type: "string", Description: "Working directory. Defaults to the repository root."},
		}, "name", "command"),
	}
}

// bgSettleDelay is how long to wait before checking the process is still alive.
//
// Long enough to catch a command that fails immediately — a missing binary, a port
// already bound — which is the failure worth reporting in the same turn. Not long
// enough to be waiting for a dev server to finish booting; that is what read_logs
// is for.
const bgSettleDelay = 700 * time.Millisecond

func (bashBgTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args bashBgArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	sb, err := env.needSandbox()
	if err != nil {
		return Result{}, err
	}
	p, err := env.needProject()
	if err != nil {
		return Result{}, err
	}

	name := strings.TrimSpace(args.Name)
	if err := validateProcessName(name); err != nil {
		return fail("%s", err), nil
	}
	if strings.TrimSpace(args.Command) == "" {
		return fail("command is required."), nil
	}
	dir, err := workDir(args.Cwd)
	if err != nil {
		return fail("%s", err), nil
	}

	// Kill the previous process of this name before its row is retired, so a
	// replacement never leaves an orphan holding the port it wants.
	replaced := false
	if prev, prevErr := env.Store.RunningBackgroundProcess(ctx, p.ID, name); prevErr == nil {
		replaced = true
		if _, killErr := killProcessGroup(ctx, sb, name); killErr != nil {
			return Result{}, killErr
		}
		if err := env.Store.SetBackgroundProcessStatus(ctx, prev.ID, store.BgStopped); err != nil {
			env.logger().Warn("retire replaced background process", "error", err, "name", name)
		}
	}

	log := logPath(name)

	pidText, launchOutput, err := launchDetached(ctx, sb, name, args.Command, dir)
	if err != nil {
		return Result{}, err
	}
	if pidText == "" {
		return fail("could not start %q: the process never reported a pid. Output was: %s",
			name, clipMiddle(launchOutput, 2<<10)), nil
	}

	// Record it before checking liveness. A process that started and then died is
	// still something the agent needs a log path for.
	bg, err := env.Store.StartBackgroundProcess(ctx, p.ID, name, args.Command, dir, log)
	if err != nil {
		return Result{}, err
	}

	select {
	case <-time.After(bgSettleDelay):
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	alive, err := processAlive(ctx, sb, pidText)
	if err != nil {
		return Result{}, err
	}
	if !alive {
		if err := env.Store.SetBackgroundProcessStatus(ctx, bg.ID, store.BgStopped); err != nil {
			env.logger().Warn("mark background process stopped", "error", err, "name", name)
		}
		tail, _ := tailFile(ctx, sb, log, 40)
		if tail == "" {
			tail = "(the log is empty)"
		}
		return Result{
			Content: fmt.Sprintf("%q exited immediately. Its log:\n\n%s", name, tail),
			Display: map[string]any{"name": name, "status": store.BgStopped, "log_path": log},
			IsError: true,
		}, nil
	}

	verb := "Started"
	if replaced {
		verb = "Replaced and started"
	}
	return Result{
		Content: fmt.Sprintf("%s %q (pid %s), logging to %s. Read it with read_logs.",
			verb, name, pidText, log),
		Display: map[string]any{
			"name": name, "pid": pidText, "status": store.BgRunning,
			"log_path": log, "cwd": relToRepo(dir), "command": args.Command,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// stop_bg
// ---------------------------------------------------------------------------

type stopBgTool struct{}

type stopBgArgs struct {
	Name string `json:"name"`
}

func (stopBgTool) Name() string { return "stop_bg" }

func (stopBgTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "stop_bg",
		Description: "Stop a background process started with bash_bg. Sends SIGTERM, then SIGKILL after a grace period.",
		Params: object(map[string]Param{
			"name": {Type: "string", Description: "The background process name."},
		}, "name"),
	}
}

func (stopBgTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args stopBgArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	sb, err := env.needSandbox()
	if err != nil {
		return Result{}, err
	}
	p, err := env.needProject()
	if err != nil {
		return Result{}, err
	}

	name := strings.TrimSpace(args.Name)
	if name == "" {
		return fail("name is required."), nil
	}

	bg, err := env.Store.RunningBackgroundProcess(ctx, p.ID, name)
	if err != nil {
		names, _ := env.Store.RunningBackgroundNames(ctx, p.ID)
		if len(names) == 0 {
			return fail("nothing named %q is running, and no background processes are.", name), nil
		}
		return fail("nothing named %q is running. Running: %s.", name, strings.Join(names, ", ")), nil
	}

	outcome, err := killProcessGroup(ctx, sb, name)
	if err != nil {
		return Result{}, err
	}
	if err := env.Store.SetBackgroundProcessStatus(ctx, bg.ID, store.BgStopped); err != nil {
		return Result{}, err
	}

	switch outcome {
	case "no-pidfile":
		// The bookkeeping is retired either way: leaving the row claiming to be
		// running would block the name forever.
		return Result{
			Content: fmt.Sprintf("%q had no pid file, so it could not be signalled — its sandbox may have been "+
				"restored or recreated since it started. The process record is now marked stopped.", name),
			Display: map[string]any{"name": name, "status": store.BgStopped, "signalled": false},
			IsError: true,
		}, nil
	case "killed":
		return ok("Stopped %q. It ignored SIGTERM, so it was killed with SIGKILL.", name), nil
	case "already-gone":
		return ok("%q had already exited. Its record is now marked stopped.", name), nil
	default:
		return ok("Stopped %q with SIGTERM.", name), nil
	}
}

// detachTimeout bounds the launcher itself, not the process it starts.
const detachTimeout = 60 * time.Second

// launchDetached starts a command in the background and returns the process-group
// id it recorded.
//
// This function looks over-built and mostly is not. Each piece below defends a
// failure that was diagnosed rather than imagined, which is why the audit's
// instinct to gut it was wrong.
//
// The command is written to its own file and run with `sh <file>` rather than
// interpolated into `sh -c '...'`. That is not tidiness: interpolating it means any
// single quote in the command collides with the quoting around it, so
// `grep 'foo bar'` silently becomes `grep foo bar` — two arguments instead of one,
// a different command, and no error anywhere. A file has no escaping at all.
//
// The rest of the shape is forced by the platform:
//
//	setsid       gives the process its own session and process group, so stop_bg
//	             can signal the whole group. A dev server started through a wrapper
//	             (npm -> node) is otherwise only half-killable.
//	echo $$      records the group leader from inside the new session. exec then
//	             replaces that shell without changing the pid.
//	redirections detach every stream. Without them the exec session's output
//	             capture never closes and this call hangs instead of returning.
//
// PIDs are tracked in a file rather than matched by pattern. `pkill -f <pattern>`
// also matches the shell running the script that invokes it, so it kills its own
// session and returns exit 137 — indistinguishable from a platform fault.
func launchDetached(ctx context.Context, sb sandbox.Sandbox, name, command, dir string) (pid, output string, err error) {
	logFile := logPath(name)
	pidFile := pidPath(name)
	cmdFile := project.LogDir + "/" + name + ".cmd"

	res, err := sb.Exec(ctx, sandbox.Command{Cmd: "mkdir -p " + shellQuote(sb.Path(project.LogDir))})
	if err != nil {
		return "", "", fmt.Errorf("create log directory: %w", err)
	}
	if res.ExitCode != 0 {
		return "", "", fmt.Errorf("create log directory: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Output()))
	}

	if err := sb.WriteFile(ctx, cmdFile, []byte(command+"\n"), 0o755); err != nil {
		return "", "", fmt.Errorf("stage command for %s: %w", name, err)
	}

	// The launcher is inlined rather than staged as a fourth file. These three paths
	// are built from a name matched against ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$, so
	// they cannot contain a quote or a space and need no escaping. The command
	// itself still goes through a file, which is the part that actually matters.
	log, pid, cmd := sb.Path(logFile), sb.Path(pidFile), sb.Path(cmdFile)
	script := fmt.Sprintf(`
rm -f %[1]s %[2]s
launch='echo $$ > %[2]s; exec sh %[3]s'
if command -v setsid >/dev/null 2>&1; then
  setsid sh -c "$launch" > %[1]s 2>&1 < /dev/null &
else
  sh -c "$launch" > %[1]s 2>&1 < /dev/null &
fi
i=0
while [ $i -lt 40 ]; do
  [ -s %[2]s ] && break
  sleep 0.1
  i=$((i+1))
done
cat %[2]s 2>/dev/null
`, log, pid, cmd)

	res, err = sb.Exec(ctx, sandbox.Command{Cmd: script, Dir: dir, Timeout: detachTimeout})
	if err != nil {
		return "", "", fmt.Errorf("start %s: %w", name, err)
	}

	output = strings.TrimSpace(res.Output())
	// The pid is the last thing printed, so a warning from the shell ahead of it
	// does not make the launch look like a failure.
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return "", output, nil
	}
	candidate := fields[len(fields)-1]
	if _, convErr := strconv.Atoi(candidate); convErr != nil {
		return "", output, nil
	}
	return candidate, output, nil
}

// killProcessGroup terminates a background process by the pid it recorded.
//
// Signals the negative pid first, which targets the whole process group: a dev
// server started through a wrapper leaves its real server child running otherwise.
// Falls back to the bare pid for a process that never became a group leader.
func killProcessGroup(ctx context.Context, sb sandbox.Sandbox, name string) (string, error) {
	pid := shellQuote(sb.Path(pidPath(name)))
	script := fmt.Sprintf(`
pid=$(cat %[1]s 2>/dev/null)
if [ -z "$pid" ]; then echo no-pidfile; exit 0; fi
if ! kill -0 "$pid" 2>/dev/null; then rm -f %[1]s; echo already-gone; exit 0; fi
kill -TERM -"$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null
i=0
while [ $i -lt 50 ]; do
  kill -0 "$pid" 2>/dev/null || break
  sleep 0.1
  i=$((i+1))
done
if kill -0 "$pid" 2>/dev/null; then
  kill -KILL -"$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null
  sleep 0.2
  echo killed
else
  echo terminated
fi
rm -f %[1]s
`, pid)

	res, err := sb.Exec(ctx, sandbox.Command{Cmd: script, Timeout: 30 * time.Second})
	if err != nil {
		return "", fmt.Errorf("stop %s: %w", name, err)
	}

	// The outcome is the last non-empty line: a shell that printed a warning first
	// should not change how the result reads.
	lines := strings.Fields(strings.TrimSpace(res.Output()))
	if len(lines) == 0 {
		return "terminated", nil
	}
	return lines[len(lines)-1], nil
}

// processAlive reports whether a pid still exists in the sandbox.
func processAlive(ctx context.Context, sb sandbox.Sandbox, pid string) (bool, error) {
	res, err := sb.Exec(ctx, sandbox.Command{
		Cmd: fmt.Sprintf(`kill -0 %s 2>/dev/null && echo alive || echo gone`, shellQuote(pid)),
	})
	if err != nil {
		return false, fmt.Errorf("check pid %s: %w", pid, err)
	}
	return strings.Contains(res.Output(), "alive"), nil
}

// tailFile returns the last n lines of a file in the sandbox.
func tailFile(ctx context.Context, sb sandbox.Sandbox, path string, n int) (string, error) {
	res, err := sb.Exec(ctx, sandbox.Command{
		Cmd: fmt.Sprintf(`tail -n %d %s 2>/dev/null`, n, shellQuote(sb.Path(path))),
	})
	if err != nil {
		return "", fmt.Errorf("tail %s: %w", path, err)
	}
	return strings.TrimRight(res.Output(), "\n"), nil
}
