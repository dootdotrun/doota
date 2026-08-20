package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dootdotrun/doot-ai/internal/project"
	"github.com/dootdotrun/doot-ai/internal/sandbox"
	"github.com/dootdotrun/doot-ai/internal/store"
)

// gitRef is a conservative check on anything interpolated into a git command as a
// revision.
//
// Everything is shell-quoted regardless, so this is not the injection defence. It
// is here because git's revision syntax accepts things like "--upload-pack=..."
// that are options rather than revisions, and rejecting those with a clear message
// beats letting git produce a baffling one.
func gitRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("%q is not a valid revision", ref)
	}
	if strings.ContainsAny(ref, " \t\n") {
		return fmt.Errorf("%q is not a valid revision", ref)
	}
	return nil
}

// currentBranch reports the checked-out branch, or "" when HEAD is detached.
func currentBranch(ctx context.Context, sb sandbox.Sandbox) (string, error) {
	res, err := sb.Exec(ctx, sandbox.Command{
		Cmd: "git rev-parse --abbrev-ref HEAD",
		Dir: project.RepoPath,
	})
	if err != nil {
		return "", fmt.Errorf("read current branch: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("read current branch: git exited %d: %s",
			res.ExitCode, strings.TrimSpace(res.Output()))
	}
	branch := strings.TrimSpace(res.Stdout)
	if branch == "HEAD" {
		return "", nil
	}
	return branch, nil
}

// requireWorkBranch refuses to proceed unless HEAD is on the doot branch.
//
// The branch invariant is enforced here, in the tool, rather than by an instruction
// in the system prompt. A prompt rule is something the model can drift from over a
// long run; a tool that will not run is not.
func requireWorkBranch(ctx context.Context, sb sandbox.Sandbox) (Result, bool, error) {
	branch, err := currentBranch(ctx, sb)
	if err != nil {
		return Result{}, false, err
	}
	if branch == store.WorkBranch {
		return Result{}, true, nil
	}
	where := "a detached HEAD"
	if branch != "" {
		where = fmt.Sprintf("branch %q", branch)
	}
	return fail("refusing to run: HEAD is on %s, and this system only ever commits to %q. "+
		"Run `git checkout %s` with bash first, and work out how HEAD moved before continuing.",
		where, store.WorkBranch, store.WorkBranch), false, nil
}

// ---------------------------------------------------------------------------
// git_diff
// ---------------------------------------------------------------------------

type gitDiffTool struct{}

type gitDiffArgs struct {
	From string `json:"from"`
	To   string `json:"to"`
	Stat bool   `json:"stat"`
}

func (gitDiffTool) Name() string { return "git_diff" }

func (gitDiffTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "git_diff",
		Description: "Show changes as a diff. With no arguments, shows uncommitted work against HEAD " +
			"during a phase, or the whole phase's work when reviewing one.",
		Params: object(map[string]Param{
			"from": {Type: "string", Description: "Starting revision. Defaults to the current phase's start commit."},
			"to":   {Type: "string", Description: "Ending revision. Defaults to HEAD."},
			"stat": {Type: "boolean", Description: "Return a per-file summary instead of the full diff."},
		}),
	}
}

func (gitDiffTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args gitDiffArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	sb, err := env.needSandbox()
	if err != nil {
		return Result{}, err
	}
	for _, ref := range []string{args.From, args.To} {
		if err := gitRef(ref); err != nil {
			return fail("%s", err), nil
		}
	}

	from := strings.TrimSpace(args.From)
	to := strings.TrimSpace(args.To)

	// Defaulting from to the phase start commit is what lets the reviewer see
	// exactly one phase's work without being told a SHA it has no way to know.
	if from == "" && env.BaseCommit != "" {
		from = env.BaseCommit
	}

	cmd := "git --no-pager diff"
	if args.Stat {
		cmd += " --stat"
	}
	var scope string
	switch {
	case from == "" && to == "":
		// No phase and no arguments: uncommitted work is the useful answer.
		cmd += " HEAD"
		scope = "uncommitted changes against HEAD"
	case from != "" && to == "":
		cmd += " " + shellQuote(from) + " HEAD"
		scope = from + "..HEAD"
	case from == "" && to != "":
		cmd += " " + shellQuote(to)
		scope = "working tree against " + to
	default:
		cmd += " " + shellQuote(from) + " " + shellQuote(to)
		scope = from + ".." + to
	}

	res, err := sb.Exec(ctx, sandbox.Command{Cmd: cmd, Dir: project.RepoPath, Timeout: 2 * time.Minute})
	if err != nil {
		return Result{}, fmt.Errorf("git diff: %w", err)
	}
	if res.ExitCode != 0 {
		return fail("git diff failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Output())), nil
	}

	body := strings.TrimRight(res.Output(), "\n")
	if body == "" {
		return ok("No changes in %s.", scope), nil
	}

	truncated := false
	if capped, cut := clip(body, maxDiffBytes); cut {
		body = capped + notice("diff is larger than %s; call git_diff with stat true for the shape of it, "+
			"then diff individual revisions", byteCount(maxDiffBytes))
		truncated = true
	}

	return Result{
		Content: fmt.Sprintf("diff of %s\n\n%s", scope, body),
		Display: map[string]any{"scope": scope, "unified": body, "stat": args.Stat, "truncated": truncated},
	}, nil
}

// ---------------------------------------------------------------------------
// git_commit
// ---------------------------------------------------------------------------

type gitCommitTool struct{}

type gitCommitArgs struct {
	Message string   `json:"message"`
	Paths   []string `json:"paths"`
}

func (gitCommitTool) Name() string { return "git_commit" }

func (gitCommitTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "git_commit",
		Description: "Stage changes and commit them locally. Never pushes. Commit small and often: " +
			"commits are the only thing protecting your work if the sandbox has to be rolled back.",
		Params: object(map[string]Param{
			"message": {Type: "string", Description: "Commit message. A subject line, optionally a blank line and a body."},
			"paths": {
				Type:        "array",
				Description: "Specific paths to stage. Defaults to every change in the working tree.",
				Items:       &Param{Type: "string"},
			},
		}, "message"),
	}
}

// commitMessagePath is where a commit message is staged inside the sandbox.
//
// Written to a file and passed with -F rather than interpolated into -m: a message
// with a body, quotes, or a backtick would otherwise need escaping that is easy to
// get subtly wrong, and the failure mode is a mangled commit message nobody notices.
const commitMessagePath = "/tmp/doot-commit-message"

func (gitCommitTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args gitCommitArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	sb, err := env.needSandbox()
	if err != nil {
		return Result{}, err
	}

	message := strings.TrimSpace(args.Message)
	if message == "" {
		return fail("message is required."), nil
	}

	if res, allowed, err := requireWorkBranch(ctx, sb); err != nil || !allowed {
		return res, err
	}

	// Stage.
	stage := "git add -A"
	if len(args.Paths) > 0 {
		quoted := make([]string, 0, len(args.Paths))
		for _, p := range args.Paths {
			abs, pathErr := repoPath(p)
			if pathErr != nil {
				return fail("%s", pathErr), nil
			}
			quoted = append(quoted, shellQuote(relToRepo(abs)))
		}
		stage = "git add -- " + strings.Join(quoted, " ")
	}

	res, err := sb.Exec(ctx, sandbox.Command{Cmd: stage, Dir: project.RepoPath, Timeout: 2 * time.Minute})
	if err != nil {
		return Result{}, fmt.Errorf("git add: %w", err)
	}
	if res.ExitCode != 0 {
		return fail("staging failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Output())), nil
	}

	// Nothing staged is worth saying plainly. An empty commit would otherwise
	// succeed and make the agent believe it had saved work it had not.
	res, err = sb.Exec(ctx, sandbox.Command{
		Cmd: "git diff --cached --quiet",
		Dir: project.RepoPath,
	})
	if err != nil {
		return Result{}, fmt.Errorf("check staged changes: %w", err)
	}
	if res.ExitCode == 0 {
		return fail("nothing to commit: there are no staged changes. Check `git status` with bash."), nil
	}

	if err := sb.WriteFile(ctx, commitMessagePath, []byte(message+"\n"), 0o644); err != nil {
		return Result{}, fmt.Errorf("write commit message: %w", err)
	}

	res, err = sb.Exec(ctx, sandbox.Command{
		Cmd:     "git commit -F " + shellQuote(sb.Path(commitMessagePath)),
		Dir:     project.RepoPath,
		Timeout: 2 * time.Minute,
	})
	if err != nil {
		return Result{}, fmt.Errorf("git commit: %w", err)
	}
	if res.ExitCode != 0 {
		return fail("commit failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Output())), nil
	}

	// Report the commit that now exists rather than echoing git's output, which
	// varies by version and configuration.
	show, err := sb.Exec(ctx, sandbox.Command{
		Cmd: "git --no-pager show --stat --oneline --no-color HEAD | head -n 40",
		Dir: project.RepoPath,
	})
	if err != nil {
		return Result{}, fmt.Errorf("describe commit: %w", err)
	}

	sha, _ := revParse(ctx, sb, "HEAD")
	return Result{
		Content: fmt.Sprintf("Committed to %s.\n\n%s", store.WorkBranch, strings.TrimRight(show.Output(), "\n")),
		Display: map[string]any{
			"branch": store.WorkBranch, "commit": sha, "message": message,
			"stat": strings.TrimRight(show.Output(), "\n"),
		},
	}, nil
}

// revParse resolves a revision to a short SHA.
func revParse(ctx context.Context, sb sandbox.Sandbox, rev string) (string, error) {
	res, err := sb.Exec(ctx, sandbox.Command{
		Cmd: "git rev-parse --short " + shellQuote(rev),
		Dir: project.RepoPath,
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("rev-parse %s: exit %d", rev, res.ExitCode)
	}
	return strings.TrimSpace(res.Stdout), nil
}

// ---------------------------------------------------------------------------
// git_push
// ---------------------------------------------------------------------------

type gitPushTool struct{}

type gitPushArgs struct {
	ForceWithLease bool `json:"force_with_lease"`
}

func (gitPushTool) Name() string { return "git_push" }

func (gitPushTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "git_push",
		Description: "Push the doot branch to origin. The only tool that touches the remote, and the branch " +
			"is not a parameter. Normally called once, when the goal is complete.",
		Params: object(map[string]Param{
			"force_with_lease": {
				Type:        "boolean",
				Description: "Force the push, but refuse if the remote moved since you last fetched. Needed after a rebase.",
			},
		}),
	}
}

func (gitPushTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args gitPushArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	sb, err := env.needSandbox()
	if err != nil {
		return Result{}, err
	}

	if res, allowed, err := requireWorkBranch(ctx, sb); err != nil || !allowed {
		return res, err
	}

	cmd := "git push"
	if args.ForceWithLease {
		cmd += " --force-with-lease"
	}
	cmd += " origin " + store.WorkBranch

	res, err := sb.Exec(ctx, sandbox.Command{Cmd: cmd, Dir: project.RepoPath, Timeout: 10 * time.Minute})
	if err != nil {
		return Result{}, fmt.Errorf("git push: %w", err)
	}

	output := strings.TrimSpace(res.Output())
	if res.ExitCode != 0 {
		// Authentication now has a real credential path, so a failure here means the
		// PAT itself is wrong rather than the transport being unsupported. Retrying
		// will not fix a bad token, so say so.
		hint := ""
		if strings.Contains(output, "could not read Username") ||
			strings.Contains(output, "Authentication failed") ||
			strings.Contains(output, "Permission denied") ||
			strings.Contains(output, "403") {
			hint = "\n\nThe GitHub token was rejected. It is installed in the sandbox as a credential-store " +
				"file, so this is a problem with the token itself: it may lack repo scope, have expired, or " +
				"not grant write access to this repository. Do not retry — report it so the operator can " +
				"replace GITHUB_TOKEN."
		}
		return fail("push failed (exit %d): %s%s", res.ExitCode, output, hint), nil
	}

	sha, _ := revParse(ctx, sb, "HEAD")
	if output == "" {
		output = "Everything up to date."
	}
	return Result{
		Content: fmt.Sprintf("Pushed %s to origin at %s.\n\n%s", store.WorkBranch, sha, output),
		Display: map[string]any{"branch": store.WorkBranch, "commit": sha, "forced": args.ForceWithLease},
	}, nil
}

// ---------------------------------------------------------------------------
// create_pr
// ---------------------------------------------------------------------------

type createPRTool struct{}

type createPRArgs struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (createPRTool) Name() string { return "create_pr" }

func (createPRTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "create_pr",
		Description: "Open a pull request from doot into the default branch. Best-effort: if it fails, the " +
			"commits are already pushed and the user can open the PR themselves, so do not treat it as fatal.",
		Params: object(map[string]Param{
			"title": {Type: "string", Description: "Pull request title."},
			"body":  {Type: "string", Description: "Pull request description."},
		}, "title", "body"),
	}
}

// githubAPI is GitHub's REST root.
//
// Called from this process with a normal HTTP client, not with curl inside the
// sandbox. The sandbox is where the *git* operations happen, because that is
// where the checkout is; a pull request is just an API call and has no reason to
// be smuggled through a shell.
const githubAPI = "https://api.github.com"

// githubTimeout bounds a single API call. Opening a pull request is one small
// request; if GitHub has not answered in this long, reporting that beats waiting.
const githubTimeout = 30 * time.Second

func (createPRTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args createPRArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	p, err := env.needProject()
	if err != nil {
		return Result{}, err
	}

	title := strings.TrimSpace(args.Title)
	if title == "" {
		return fail("title is required."), nil
	}
	if env.GitHubToken == "" {
		return fail("no GitHub token is configured, so a pull request cannot be opened. " +
			"The push already succeeded; tell the user to open it themselves, or to set GITHUB_TOKEN and redeploy."), nil
	}

	owner, repo, failure := parseGitHubRepo(p.RepoURL)
	if failure != "" {
		return fail("%s", failure), nil
	}

	// An already-open PR for this branch is the expected case on a second goal, not
	// an error: the branch is permanent, so the first PR stays open and keeps
	// collecting commits.
	if existing := findOpenPR(ctx, env.GitHubToken, owner, repo); existing != "" {
		return Result{
			Content: fmt.Sprintf("A pull request for %s is already open: %s\nThe new commits are on it already.",
				store.WorkBranch, existing),
			Display: map[string]any{"url": existing, "created": false},
		}, nil
	}

	payload, err := json.Marshal(map[string]string{
		"title": title,
		"body":  args.Body,
		"head":  store.WorkBranch,
		"base":  p.DefaultBranch,
	})
	if err != nil {
		return Result{}, fmt.Errorf("encode pull request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls", githubAPI, owner, repo)
	status, body, err := githubCall(ctx, env.GitHubToken, http.MethodPost, endpoint, payload)
	if err != nil {
		// A transport failure here is not infrastructure the run should stall on:
		// the commits are already pushed. Report it and let the agent carry on.
		return Result{
			Content: "Could not reach GitHub to open a pull request: " + err.Error() +
				"\n\nThe push itself succeeded, so this is not blocking — report it and carry on.",
			Display: map[string]any{"created": false},
			IsError: true,
		}, nil
	}

	if status == http.StatusCreated {
		var created struct {
			HTMLURL string `json:"html_url"`
			Number  int    `json:"number"`
		}
		if json.Unmarshal(body, &created) == nil && created.HTMLURL != "" {
			return Result{
				Content: fmt.Sprintf("Opened pull request #%d: %s", created.Number, created.HTMLURL),
				Display: map[string]any{"url": created.HTMLURL, "number": created.Number, "created": true},
			}, nil
		}
		return ok("Pull request created, but the response did not include a URL."), nil
	}

	// Never fatal. PRs are optional; the commits are already on the remote and the
	// human can open one in a browser in ten seconds.
	hint := ""
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		hint = " The token was rejected — it may lack repo scope or have expired. Do not retry; report it."
	}
	return Result{
		Content: fmt.Sprintf("Could not open a pull request (HTTP %d).%s The push itself succeeded, so this is not "+
			"blocking — report it and carry on.\n\n%s", status, hint, clipMiddle(string(body), 4<<10)),
		Display: map[string]any{"status": status, "created": false},
		IsError: true,
	}, nil
}

// findOpenPR returns the URL of an already-open PR for the work branch, or "".
//
// Every failure is silent and returns "": the only consequence of getting this
// wrong is attempting a creation that reports its own error, which is a better
// outcome than refusing to try.
func findOpenPR(ctx context.Context, token, owner, repo string) string {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&head=%s:%s",
		githubAPI, owner, repo, owner, store.WorkBranch)

	status, body, err := githubCall(ctx, token, http.MethodGet, endpoint, nil)
	if err != nil || status != http.StatusOK {
		return ""
	}
	var open []struct {
		HTMLURL string `json:"html_url"`
	}
	if json.Unmarshal(body, &open) != nil || len(open) == 0 {
		return ""
	}
	return open[0].HTMLURL
}

// githubCall makes one authenticated REST call and returns the status and body.
func githubCall(ctx context.Context, token, method, endpoint string, body []byte) (int, []byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, githubTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(callCtx, method, endpoint, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	// Bounded: an error body from GitHub is small, and a surprise is not a reason
	// to read an unbounded response into memory.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, payload, nil
}

// parseGitHubRepo extracts owner and repository from a remote URL.
func parseGitHubRepo(remote string) (owner, repo, failure string) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(remote), ".git")

	switch {
	case strings.HasPrefix(trimmed, "git@"):
		// git@github.com:owner/repo
		_, rest, found := strings.Cut(trimmed, ":")
		if !found {
			return "", "", fmt.Sprintf("could not work out the owner and repository from %q.", remote)
		}
		trimmed = rest
	case strings.Contains(trimmed, "://"):
		u, err := url.Parse(trimmed)
		if err != nil {
			return "", "", fmt.Sprintf("could not parse the repository URL %q.", remote)
		}
		if !strings.Contains(u.Host, "github.com") {
			return "", "", fmt.Sprintf("%q is not a github.com repository, so there is no pull request API to call.", u.Host)
		}
		trimmed = strings.TrimPrefix(u.Path, "/")
	default:
		return "", "", fmt.Sprintf("%q is a local path, not a GitHub repository, so there is nowhere to open a pull request.", remote)
	}

	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Sprintf("could not work out the owner and repository from %q.", remote)
	}
	return parts[0], parts[1], ""
}
