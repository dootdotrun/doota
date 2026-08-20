package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/dootdotrun/doot-ai/internal/sandbox"
)

// ---------------------------------------------------------------------------
// write_file
// ---------------------------------------------------------------------------

type writeFileTool struct{}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (writeFileTool) Name() string { return "write_file" }

func (writeFileTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "write_file",
		Description: "Create a file or replace its entire contents, creating parent directories as needed. " +
			"Prefer edit_file for a file that already exists: rewriting a long file to change a few lines " +
			"wastes output tokens and produces a diff nobody can review.",
		Params: object(map[string]Param{
			"path":    {Type: "string", Description: "Path relative to the repository root."},
			"content": {Type: "string", Description: "The complete new contents of the file."},
		}, "path", "content"),
	}
}

func (writeFileTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args writeFileArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	sb, err := env.needSandbox()
	if err != nil {
		return Result{}, err
	}
	abs, err := repoPath(args.Path)
	if err != nil {
		return fail("%s", err), nil
	}

	kind, size, err := statPath(ctx, sb, abs)
	if err != nil {
		return Result{}, err
	}
	if kind == "dir" {
		return fail("%s is a directory.", relToRepo(abs)), nil
	}
	if kind == "other" {
		return fail("%s exists and is not a regular file.", relToRepo(abs)), nil
	}

	// Read the previous contents so the diff shows what actually changed rather
	// than presenting every line of an existing file as new.
	var old string
	if kind == "file" && size <= readCeilingBytes {
		raw, readErr := sb.ReadFile(ctx, abs)
		if readErr != nil {
			return Result{}, fmt.Errorf("read %s before writing: %w", relToRepo(abs), readErr)
		}
		old = string(raw)
	}

	// The Sprites filesystem API does not create parents, and the local provider
	// does. Doing it explicitly makes the two behave the same.
	if dir := path.Dir(abs); dir != "" && dir != "." && dir != "/" {
		res, mkErr := sb.Exec(ctx, sandbox.Command{Cmd: "mkdir -p " + shellQuote(sb.Path(dir))})
		if mkErr != nil {
			return Result{}, fmt.Errorf("create parent of %s: %w", relToRepo(abs), mkErr)
		}
		if res.ExitCode != 0 {
			return fail("could not create directory %s: %s", path.Dir(relToRepo(abs)), strings.TrimSpace(res.Output())), nil
		}
	}

	if err := sb.WriteFile(ctx, abs, []byte(args.Content), 0o644); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", relToRepo(abs), err)
	}

	diff := fileDiff(ctx, sb, abs, kind != "file", args.Content)
	env.emit(Event{Type: EventToolDiff, Tool: "write_file", Payload: diff})

	if old == args.Content {
		return Result{
			Content: fmt.Sprintf("%s was already exactly this content; nothing changed.", relToRepo(abs)),
			Display: diff,
		}, nil
	}

	verb := "Wrote"
	if kind != "file" {
		verb = "Created"
	}
	return Result{
		Content: fmt.Sprintf("%s %s — %s.", verb, relToRepo(abs), diff.Stat()),
		Display: diff,
	}, nil
}

// ---------------------------------------------------------------------------
// edit_file
// ---------------------------------------------------------------------------

type editFileTool struct{}

type editFileArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (editFileTool) Name() string { return "edit_file" }

func (editFileTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "edit_file",
		Description: "Replace an exact string in a file. This is the preferred way to change existing code. " +
			"Fails if old_string is not found, or if it appears more than once and replace_all is false.",
		Params: object(map[string]Param{
			"path":        {Type: "string", Description: "Path relative to the repository root."},
			"old_string":  {Type: "string", Description: "Exact text to replace, including indentation."},
			"new_string":  {Type: "string", Description: "Replacement text."},
			"replace_all": {Type: "boolean", Description: "Replace every occurrence instead of requiring exactly one."},
		}, "path", "old_string", "new_string"),
	}
}

func (editFileTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args editFileArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	sb, err := env.needSandbox()
	if err != nil {
		return Result{}, err
	}
	abs, err := repoPath(args.Path)
	if err != nil {
		return fail("%s", err), nil
	}
	if args.OldString == "" {
		return fail("old_string is required. Use write_file to create a file from nothing."), nil
	}
	if args.OldString == args.NewString {
		return fail("old_string and new_string are identical, so this edit would do nothing."), nil
	}

	kind, size, err := statPath(ctx, sb, abs)
	if err != nil {
		return Result{}, err
	}
	switch kind {
	case "missing":
		return fail("%s does not exist. Use write_file to create it.", relToRepo(abs)), nil
	case "dir":
		return fail("%s is a directory.", relToRepo(abs)), nil
	case "other":
		return fail("%s is not a regular file.", relToRepo(abs)), nil
	}
	if size > readCeilingBytes {
		return fail("%s is %s, too large to edit in memory. Use bash with sed for a file this size.",
			relToRepo(abs), byteCount(size)), nil
	}

	raw, err := sb.ReadFile(ctx, abs)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", relToRepo(abs), err)
	}
	old := string(raw)

	// Naming the match count is the whole point of these two failures: it tells the
	// agent whether to add surrounding context or to look for a different file,
	// which "edit failed" does not.
	count := strings.Count(old, args.OldString)
	switch {
	case count == 0:
		hint := ""
		if strings.Contains(args.OldString, "\t") {
			hint = " The text contains a tab; check whether the file uses spaces."
		} else if trimmed := strings.TrimSpace(args.OldString); trimmed != args.OldString && strings.Contains(old, trimmed) {
			hint = " The trimmed text does appear, so the leading or trailing whitespace is wrong."
		}
		return fail("old_string was not found in %s (0 matches). Read the file and copy the exact text, including indentation.%s",
			relToRepo(abs), hint), nil
	case count > 1 && !args.ReplaceAll:
		return fail("old_string matches %d times in %s. Add surrounding lines to make it unique, or set replace_all true to change all %d.",
			count, relToRepo(abs), count), nil
	}

	replaced := old
	if args.ReplaceAll {
		replaced = strings.ReplaceAll(old, args.OldString, args.NewString)
	} else {
		replaced = strings.Replace(old, args.OldString, args.NewString, 1)
	}

	if err := sb.WriteFile(ctx, abs, []byte(replaced), 0o644); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", relToRepo(abs), err)
	}

	diff := fileDiff(ctx, sb, abs, false, replaced)
	env.emit(Event{Type: EventToolDiff, Tool: "edit_file", Payload: diff})

	occurrences := "1 occurrence"
	if count > 1 {
		occurrences = fmt.Sprintf("%d occurrences", count)
	}
	return Result{
		Content: fmt.Sprintf("Replaced %s in %s — %s.\n\n%s", occurrences, relToRepo(abs), diff.Stat(), diff.Unified),
		Display: diff,
	}, nil
}

// ---------------------------------------------------------------------------
// bash
// ---------------------------------------------------------------------------

type bashTool struct{}

type bashArgs struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	Timeout int    `json:"timeout"`
}

func (bashTool) Name() string { return "bash" }

func (bashTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "bash",
		Description: "Run a shell command in the sandbox and return its combined output and exit code. " +
			"Stateless between calls: there is no persistent working directory and no carried-over " +
			"environment, so pass cwd explicitly. For anything that does not exit, use bash_bg.",
		Params: object(map[string]Param{
			"command": {Type: "string", Description: "Shell command line to run."},
			"cwd":     {Type: "string", Description: "Working directory. Defaults to the repository root."},
			"timeout": {Type: "integer", Description: "Seconds before the command is killed. Default 120, maximum 1800."},
		}, "command"),
	}
}

// bash timeout bounds, in seconds.
const (
	bashDefaultTimeout = 120
	bashMaxTimeout     = 1800
)

func (bashTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args bashArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	if strings.TrimSpace(args.Command) == "" {
		return fail("command is required."), nil
	}
	sb, err := env.needSandbox()
	if err != nil {
		return Result{}, err
	}
	dir, err := workDir(args.Cwd)
	if err != nil {
		return fail("%s", err), nil
	}

	seconds := args.Timeout
	if seconds <= 0 {
		seconds = bashDefaultTimeout
	}
	if seconds > bashMaxTimeout {
		seconds = bashMaxTimeout
	}

	// Output is accumulated within a fixed budget and streamed to the UI as it
	// arrives, so a long build is watchable on the phone without the transcript
	// having to hold all of it.
	buf := newBoundedBuffer(maxBashBytes)
	var sink io.Writer = buf
	if env != nil && env.Emit != nil {
		sink = io.MultiWriter(buf, emitWriter{env: env, tool: "bash"})
	}

	started := time.Now()
	exitCode, err := sb.ExecStream(ctx, sandbox.Command{
		Cmd:     args.Command,
		Dir:     dir,
		Timeout: time.Duration(seconds) * time.Second,
	}, sink, sink)
	elapsed := time.Since(started)

	if err != nil {
		// A missing sandbox is an infrastructure failure, not a command failure
		// the model can fix. Preserve it for the agent's recovery boundary.
		if errors.Is(err, sandbox.ErrNotFound) {
			return Result{}, err
		}
		// A timeout arrives as a transport error with output already captured, and
		// the agent can act on that, so it is reported rather than escalated.
		return Result{
			Content: fmt.Sprintf("Command did not complete after %s: %v\ncwd: %s\n\n%s",
				elapsed.Round(time.Millisecond), err, relToRepo(dir), buf.String()),
			Display: map[string]any{"command": args.Command, "cwd": relToRepo(dir), "failed": true},
			IsError: true,
		}, nil
	}

	output := strings.TrimRight(buf.String(), "\n")

	// Display carries metadata only, deliberately not a copy of the output.
	//
	// Display exists for payloads Content cannot express — a structured diff, a
	// findings list. Command output is already plain text in the right shape, so
	// duplicating it here would double the tool_call_log row and the event payload
	// for every command the agent ever runs, and buy the UI nothing it cannot get
	// from Content.
	display := map[string]any{
		"command":     args.Command,
		"cwd":         relToRepo(dir),
		"exit_code":   exitCode,
		"duration_ms": elapsed.Milliseconds(),
		"bytes":       len(output),
	}

	var out strings.Builder
	if output == "" {
		out.WriteString("(no output)")
	} else {
		out.WriteString(output)
	}
	if exitCode != 0 {
		fmt.Fprintf(&out, "\n\n[exit %d after %s]", exitCode, elapsed.Round(time.Millisecond))
	}

	return Result{Content: out.String(), Display: display, IsError: exitCode != 0}, nil
}

// emitWriter forwards command output to the UI as it is produced.
//
// Only wired up when something is listening: turning every chunk into a string for
// an event nobody consumes is pure waste outside a live agent run.
type emitWriter struct {
	env  *Env
	tool string
}

func (w emitWriter) Write(p []byte) (int, error) {
	w.env.emit(Event{Type: EventToolOutput, Tool: w.tool, Payload: string(p)})
	return len(p), nil
}
