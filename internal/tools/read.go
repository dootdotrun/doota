package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/project"
	"github.com/dootdotrun/doot-ai/internal/sandbox"
)

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

type readFileTool struct{}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func (readFileTool) Name() string { return "read_file" }

func (readFileTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "read_file",
		Description: "Read a file from the repository, returned with line numbers. " +
			"Caps at 2000 lines or 250 KB, whichever comes first, and says so explicitly when it truncates.",
		Params: object(map[string]Param{
			"path":   {Type: "string", Description: "Path relative to the repository root."},
			"offset": {Type: "integer", Description: "Zero-indexed first line to read. Default 0."},
			"limit":  {Type: "integer", Description: "Maximum lines to read. Default and maximum 2000."},
		}, "path"),
	}
}

func (readFileTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args readFileArgs
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
	switch kind {
	case "missing":
		return fail("%s does not exist.", relToRepo(abs)), nil
	case "dir":
		return fail("%s is a directory. Use list_dir to see what is in it.", relToRepo(abs)), nil
	case "other":
		return fail("%s is not a regular file.", relToRepo(abs)), nil
	}

	// Refusing beats buffering. The provider's ReadFile has no streaming form, so
	// a huge file would be pulled into memory whole to show 2000 lines of it.
	if size > readCeilingBytes {
		return fail("%s is %s, larger than the %s read limit. Use bash with sed or grep to pull out the part you need.",
			relToRepo(abs), byteCount(size), byteCount(readCeilingBytes)), nil
	}

	raw, err := sb.ReadFile(ctx, abs)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", relToRepo(abs), err)
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return fail("%s looks like a binary file (%s). Reading it as text would be meaningless.",
			relToRepo(abs), byteCount(len(raw))), nil
	}

	offset := max(args.Offset, 0)
	limit := args.Limit
	if limit <= 0 || limit > maxFileLines {
		limit = maxFileLines
	}

	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(raw) == 0 {
		return Result{
			Content: fmt.Sprintf("%s is empty (0 bytes).", relToRepo(abs)),
			Display: map[string]any{"path": relToRepo(abs), "lines": 0, "bytes": 0},
		}, nil
	}
	total := len(lines)
	if offset >= total {
		return fail("offset %d is past the end of %s, which has %d lines.", offset, relToRepo(abs), total), nil
	}

	end := min(offset+limit, total)
	body := strings.Join(lines[offset:end], "\n")

	var notices []string
	if capped, cut := clip(body, maxFileBytes); cut {
		body = capped
		// Recount so the header range matches what is actually shown.
		end = offset + len(strings.Split(body, "\n"))
		notices = append(notices, fmt.Sprintf("stopped at %s", byteCount(maxFileBytes)))
	}
	if end < total {
		notices = append(notices, fmt.Sprintf("showing lines %d-%d of %d; read on with offset %d",
			offset+1, end, total, end))
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%s (lines %d-%d of %d)\n", relToRepo(abs), offset+1, end, total)
	out.WriteString(numberLines(body, offset+1))
	if len(notices) > 0 {
		out.WriteString(notice("%s", strings.Join(notices, "; ")))
	}

	return Result{
		Content: out.String(),
		Display: map[string]any{
			"path": relToRepo(abs), "from": offset + 1, "to": end,
			"lines": total, "bytes": size,
		},
	}, nil
}

// statPath reports whether a path exists and what it is.
//
// One exec rather than a read that fails: distinguishing "no such file" from "that
// is a directory" is the difference between the agent fixing a typo and the agent
// retrying the same call.
func statPath(ctx context.Context, sb sandbox.Sandbox, abs string) (kind string, size int, err error) {
	quoted := shellQuote(sb.Path(abs))
	res, err := sb.Exec(ctx, sandbox.Command{
		Cmd: fmt.Sprintf(
			`if [ -d %[1]s ]; then echo dir; elif [ -f %[1]s ]; then echo "file $(wc -c < %[1]s | tr -d " ")"; elif [ -e %[1]s ]; then echo other; else echo missing; fi`,
			quoted),
	})
	if err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", relToRepo(abs), err)
	}

	fields := strings.Fields(strings.TrimSpace(res.Output()))
	if len(fields) == 0 {
		return "", 0, fmt.Errorf("stat %s: no output", relToRepo(abs))
	}
	if fields[0] == "file" && len(fields) > 1 {
		n, convErr := strconv.Atoi(fields[1])
		if convErr != nil {
			return "", 0, fmt.Errorf("stat %s: unreadable size %q", relToRepo(abs), fields[1])
		}
		return "file", n, nil
	}
	return fields[0], 0, nil
}

// ---------------------------------------------------------------------------
// list_dir
// ---------------------------------------------------------------------------

type listDirTool struct{}

type listDirArgs struct {
	Path  string `json:"path"`
	Depth int    `json:"depth"`
}

func (listDirTool) Name() string { return "list_dir" }

func (listDirTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "list_dir",
		Description: "List directory contents, respecting .gitignore. Subtrees deeper than the " +
			"requested depth are collapsed to a single entry with a count. Empty directories are not listed.",
		Params: object(map[string]Param{
			"path":  {Type: "string", Description: "Path relative to the repository root. Defaults to the root."},
			"depth": {Type: "integer", Description: "Levels to descend, 1 to 3. Default 1."},
		}),
	}
}

func (listDirTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args listDirArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	sb, err := env.needSandbox()
	if err != nil {
		return Result{}, err
	}

	target := project.RepoPath
	if strings.TrimSpace(args.Path) != "" {
		if target, err = repoPath(args.Path); err != nil {
			return fail("%s", err), nil
		}
	}

	depth := args.Depth
	if depth <= 0 {
		depth = 1
	}
	// A depth-3 listing of node_modules is thousands of useless tokens, which is
	// the whole reason this is bounded rather than recursive by default.
	if depth > 3 {
		depth = 3
	}

	kind, _, err := statPath(ctx, sb, target)
	if err != nil {
		return Result{}, err
	}
	switch kind {
	case "missing":
		return fail("%s does not exist.", relToRepo(target)), nil
	case "file":
		return fail("%s is a file, not a directory. Use read_file.", relToRepo(target)), nil
	}

	paths, err := listPaths(ctx, sb, target)
	if err != nil {
		return Result{}, err
	}
	if len(paths) == 0 {
		return ok("%s is empty, or everything in it is ignored by git.", relToRepo(target)), nil
	}

	entries := collapse(paths, depth)
	total := len(entries)
	if total > maxDirEntries {
		entries = entries[:maxDirEntries]
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%s (depth %d, %d entries)\n", relToRepo(target), depth, total)
	for _, e := range entries {
		if e.dir {
			fmt.Fprintf(&out, "  %s/", e.name)
			if e.count > 0 {
				fmt.Fprintf(&out, "  (%d entries)", e.count)
			}
			out.WriteByte('\n')
			continue
		}
		fmt.Fprintf(&out, "  %s\n", e.name)
	}
	if total > maxDirEntries {
		out.WriteString(notice("showing %d of %d entries; narrow the path to see the rest", maxDirEntries, total))
	}
	return Result{
		Content: out.String(),
		Display: map[string]any{"path": relToRepo(target), "depth": depth, "entries": total},
	}, nil
}

// listPaths returns paths under dir, relative to dir.
//
// git ls-files applies .gitignore exactly as git does, including nested ignore
// files and the global excludes file. Reimplementing that matching would be a
// source of quiet disagreement with what the repository actually tracks.
//
// There was a find-based fallback for directories outside a work tree. Every path
// this tool accepts is inside the clone, so it existed for a case that cannot
// happen — along with a note in the output explaining that .gitignore had not been
// applied, which the operator could therefore never see.
func listPaths(ctx context.Context, sb sandbox.Sandbox, dir string) (paths []string, err error) {
	res, err := sb.Exec(ctx, sandbox.Command{
		// core.quotePath=false keeps non-ASCII names readable instead of octal-escaped.
		Cmd: `git -c core.quotePath=false ls-files --cached --others --exclude-standard --full-name -- .`,
		Dir: dir,
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", relToRepo(dir), err)
	}

	if res.ExitCode != 0 {
		return nil, fmt.Errorf("list %s: git exited %d: %s",
			relToRepo(dir), res.ExitCode, strings.TrimSpace(res.Output()))
	}

	// --full-name yields repo-root-relative paths, so strip the prefix of the
	// directory being listed to get paths relative to it.
	prefix := strings.TrimPrefix(strings.TrimPrefix(dir, project.RepoPath), "/")
	for _, line := range strings.Split(res.Output(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rel := line
		if prefix != "" {
			if !strings.HasPrefix(line, prefix+"/") {
				continue
			}
			rel = strings.TrimPrefix(line, prefix+"/")
		}
		paths = append(paths, rel)
	}
	return paths, nil
}

type listEntry struct {
	name  string
	dir   bool
	count int
}

// collapse folds a flat path list into entries no deeper than depth, replacing
// deeper subtrees with a directory entry carrying a count.
func collapse(paths []string, depth int) []listEntry {
	seen := map[string]*listEntry{}
	order := []string{}

	add := func(name string, isDir bool) *listEntry {
		if e, found := seen[name]; found {
			return e
		}
		e := &listEntry{name: name, dir: isDir}
		seen[name] = e
		order = append(order, name)
		return e
	}

	for _, p := range paths {
		trimmed := strings.TrimSuffix(p, "/")
		if trimmed == "" {
			continue
		}
		parts := strings.Split(trimmed, "/")

		if len(parts) <= depth {
			e := add(trimmed, strings.HasSuffix(p, "/"))
			if strings.HasSuffix(p, "/") {
				e.dir = true
			}
			// Every ancestor is a directory that exists.
			for i := 1; i < len(parts); i++ {
				add(strings.Join(parts[:i], "/"), true).dir = true
			}
			continue
		}

		// Deeper than requested: collapse at the boundary and count what is inside.
		boundary := strings.Join(parts[:depth], "/")
		e := add(boundary, true)
		e.dir = true
		e.count++
		for i := 1; i < depth; i++ {
			add(strings.Join(parts[:i], "/"), true).dir = true
		}
	}

	out := make([]listEntry, 0, len(order))
	for _, name := range order {
		out = append(out, *seen[name])
	}

	// Lexicographic order on the full path already reads as a tree: a parent sorts
	// immediately before everything inside it, and siblings group together. The sort
	// is needed at all only because ancestors are added to the map when a deeper
	// path first mentions them, which is not the order they should print in.
	//
	// There was a component-wise comparator here that additionally put directories
	// before files at each level. Twenty lines of index arithmetic, with a branch
	// inferring whether an entry was a directory from how many components followed
	// it, for an ordering nicety inside a listing that already shows a trailing
	// slash on every directory.
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// ---------------------------------------------------------------------------
// search
// ---------------------------------------------------------------------------

type searchTool struct{}

type searchArgs struct {
	Pattern string `json:"pattern"`
	Glob    string `json:"glob"`
	Literal bool   `json:"literal"`
}

func (searchTool) Name() string { return "search" }

func (searchTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "search",
		Description: "Search the repository for a regular expression, returning file, line number, " +
			"and the matched line. Capped at 100 matches.",
		Params: object(map[string]Param{
			"pattern": {Type: "string", Description: "Regular expression to search for."},
			"glob":    {Type: "string", Description: `Restrict to matching files, for example "**/*.go".`},
			"literal": {Type: "boolean", Description: "Treat the pattern as a literal string rather than a regex."},
		}, "pattern"),
	}
}

func (searchTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args searchArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return fail("pattern is required."), nil
	}
	sb, err := env.needSandbox()
	if err != nil {
		return Result{}, err
	}

	pattern := shellQuote(args.Pattern)

	rg := "rg --line-number --no-heading --color=never --glob " + shellQuote("!.git")
	if args.Literal {
		rg += " --fixed-strings"
	}
	if args.Glob != "" {
		rg += " --glob " + shellQuote(args.Glob)
	}
	rg += " --regexp " + pattern + " ."

	// Output is capped in the shell so a match-everything pattern cannot ship
	// megabytes over the wire. The exit code is deliberately not consulted: rg
	// exits non-zero for "no matches", and piping would report head's status
	// anyway. Emptiness is the signal, and unparseable output is the error.
	//
	// There was a grep fallback here for an image without ripgrep, with its own
	// flag translation and a note that --include matches a basename so a leading
	// **/ had to be stripped. The setup script installs ripgrep and reports whether
	// it succeeded; a second search implementation with subtly different glob
	// semantics is a worse outcome than a clear failure.
	res, err := sb.Exec(ctx, sandbox.Command{
		Cmd: fmt.Sprintf(`%s 2>&1 | head -n %d`, rg, maxSearchHits*2),
		Dir: project.RepoPath,
	})
	if err != nil {
		return Result{}, fmt.Errorf("search: %w", err)
	}

	output := strings.TrimSpace(res.Output())
	if output == "" {
		return ok("No matches for %s.", args.Pattern), nil
	}

	type hit struct {
		file, line, text string
	}
	var hits []hit
	var unparsed []string
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		file, rest, found := strings.Cut(line, ":")
		if !found {
			unparsed = append(unparsed, line)
			continue
		}
		num, text, found := strings.Cut(rest, ":")
		if !found || num == "" {
			unparsed = append(unparsed, line)
			continue
		}
		if _, convErr := strconv.Atoi(num); convErr != nil {
			unparsed = append(unparsed, line)
			continue
		}
		hits = append(hits, hit{strings.TrimPrefix(file, "./"), num, text})
	}

	// Nothing matched the expected shape, so the output is a diagnostic — an
	// invalid regex, most likely. Report it rather than presenting it as results.
	if len(hits) == 0 {
		return fail("search failed: %s", clipMiddle(output, 2<<10)), nil
	}

	truncated := false
	if len(hits) > maxSearchHits {
		hits = hits[:maxSearchHits]
		truncated = true
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%d matches for %s\n", len(hits), args.Pattern)
	for _, h := range hits {
		text := h.text
		if len(text) > 300 {
			text = text[:300] + " …"
		}
		fmt.Fprintf(&out, "%s:%s: %s\n", h.file, h.line, strings.TrimRight(text, "\r"))
	}
	if truncated {
		out.WriteString(notice("stopped at %d matches; narrow the pattern or pass a glob", maxSearchHits))
	}
	if len(unparsed) > 0 {
		fmt.Fprintf(&out, "\n[search also reported: %s]", clipMiddle(strings.Join(unparsed, "; "), 1<<10))
	}

	return Result{
		Content: out.String(),
		Display: map[string]any{"pattern": args.Pattern, "matches": len(hits), "truncated": truncated},
	}, nil
}

// ---------------------------------------------------------------------------
// read_logs
// ---------------------------------------------------------------------------

type readLogsTool struct{}

type readLogsArgs struct {
	Name string `json:"name"`
	Tail int    `json:"tail"`
}

func (readLogsTool) Name() string { return "read_logs" }

func (readLogsTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "read_logs",
		Description: "Read the output of a background process started with bash_bg. " +
			"This is how you find out whether your dev server started or is crash-looping. " +
			"An empty log does not always mean silence: many runtimes buffer output when it is " +
			"not a terminal, so start them unbuffered if you need to see it promptly.",
		Params: object(map[string]Param{
			"name": {Type: "string", Description: "The background process name."},
			"tail": {Type: "integer", Description: "Lines from the end of the log. Default 200."},
		}, "name"),
	}
}

func (readLogsTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args readLogsArgs
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

	tail := args.Tail
	if tail <= 0 {
		tail = defaultLogTail
	}
	if tail > maxLogTail {
		tail = maxLogTail
	}

	// The latest process with this name, running or not: the log of one that has
	// already died is exactly what is wanted when a dev server will not stay up.
	bg, err := env.Store.LatestBackgroundProcess(ctx, p.ID, name)
	if err != nil {
		names, _ := env.Store.RunningBackgroundNames(ctx, p.ID)
		if len(names) == 0 {
			return fail("no background process named %q, and none are running. Start one with bash_bg.", name), nil
		}
		return fail("no background process named %q. Running: %s.", name, strings.Join(names, ", ")), nil
	}

	res, err := sb.Exec(ctx, sandbox.Command{
		Cmd: fmt.Sprintf(`tail -n %d %s 2>/dev/null`, tail, shellQuote(sb.Path(bg.LogPath))),
	})
	if err != nil {
		return Result{}, fmt.Errorf("read logs for %s: %w", name, err)
	}

	state := bg.Status
	if !bg.IsRunning() && bg.StoppedAt.Valid {
		state = fmt.Sprintf("%s at %s", bg.Status, bg.StoppedAt.Time.UTC().Format("15:04:05"))
	}

	body := strings.TrimRight(res.Output(), "\n")
	if body == "" {
		return Result{
			Content: fmt.Sprintf("%s (%s) has produced no output yet.\ncommand: %s", name, state, bg.Command),
			Display: map[string]any{"name": name, "status": bg.Status, "lines": 0},
		}, nil
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%s (%s), last %d lines of %s\ncommand: %s\n\n", name, state, tail, bg.LogPath, bg.Command)
	out.WriteString(clipMiddle(body, maxBashBytes))

	return Result{
		Content: out.String(),
		Display: map[string]any{
			"name": name, "status": bg.Status, "log_path": bg.LogPath,
			"lines": strings.Count(body, "\n") + 1,
		},
	}, nil
}
