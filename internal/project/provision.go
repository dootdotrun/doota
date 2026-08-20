package project

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dootdotrun/doot-ai/internal/sandbox"
	"github.com/dootdotrun/doot-ai/internal/store"
)

// provision brings a project from a bare row to a ready sandbox with the repo
// cloned and the doot branch checked out.
func (s *Service) provision(ctx context.Context, projectID string) error {
	p, err := s.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	cfg, err := s.store.LoadConfig(ctx)
	if err != nil {
		return err
	}

	s.logLine(ctx, projectID, "=== doot setup ===")
	s.logLine(ctx, projectID, "provider: "+s.provider.Kind())
	s.logLine(ctx, projectID, "repo:     "+p.RepoURL)

	sb, err := s.ensureSandbox(ctx, p)
	if err != nil {
		return err
	}

	if err := s.runSetupScript(ctx, p, sb, cfg); err != nil {
		return err
	}
	if err := s.configureGit(ctx, p, sb, cfg); err != nil {
		return err
	}
	if err := s.cloneRepo(ctx, p, sb); err != nil {
		return err
	}
	if err := s.checkoutWorkBranch(ctx, p, sb); err != nil {
		return err
	}
	s.installDependencies(ctx, p, sb)

	return nil
}

// ensureSandbox creates the sandbox if the project does not have one yet.
func (s *Service) ensureSandbox(ctx context.Context, p *store.Project) (sandbox.Sandbox, error) {
	if name := p.Sandbox(); name != "" {
		sb, err := s.provider.Get(ctx, name)
		if err == nil {
			s.logLine(ctx, p.ID, "reusing sandbox "+name)
			return sb, nil
		}
		s.logLine(ctx, p.ID, "previous sandbox "+name+" is gone; creating a new one")
	}

	name := sandboxName(p.ID)
	s.logLine(ctx, p.ID, "creating sandbox "+name)

	sb, err := s.provider.Create(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}

	// Persist the name before anything else can fail. A sandbox with no database
	// record is an orphan nobody will ever find, let alone clean up.
	if err := s.store.SetSandbox(ctx, p.ID, name); err != nil {
		return nil, err
	}
	p.SandboxID.String, p.SandboxID.Valid = name, true

	if url, err := sb.URL(ctx); err == nil && url != "" {
		s.logLine(ctx, p.ID, "sandbox url: "+url)
	}
	return sb, nil
}

// sandboxName derives a stable, DNS-safe name from the project id.
func sandboxName(projectID string) string {
	compact := strings.ReplaceAll(projectID, "-", "")
	if len(compact) > 12 {
		compact = compact[:12]
	}
	return "doot-" + compact
}

// runSetupScript installs the toolchain. Failure is a warning, not fatal: the
// image may already have everything, and the clone step will fail loudly if git
// really is missing.
func (s *Service) runSetupScript(ctx context.Context, p *store.Project, sb sandbox.Sandbox, cfg store.AppConfig) error {
	script := cfg.String("sandbox.setup_script")
	if strings.TrimSpace(script) == "" {
		s.logLine(ctx, p.ID, "no setup script configured; skipping")
		return nil
	}

	s.step(ctx, p.ID, "setup script")
	if err := sb.WriteFile(ctx, SetupPath, []byte(script), 0o755); err != nil {
		return fmt.Errorf("write setup script: %w", err)
	}
	res, err := s.run(ctx, p.ID, sb, sandbox.Command{
		Cmd:     "sh " + shellQuote(sb.Path(SetupPath)),
		Timeout: 10 * time.Minute,
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		s.logLine(ctx, p.ID, fmt.Sprintf("warning: setup script exited %d; continuing", res.ExitCode))
	}
	return nil
}

// configureGit sets the commit identity and installs the GitHub credential.
//
// Runs before the clone, so a private repository clones on the first attempt
// rather than being a separate failure to discover later.
func (s *Service) configureGit(ctx context.Context, p *store.Project, sb sandbox.Sandbox, cfg store.AppConfig) error {
	s.step(ctx, p.ID, "git identity")

	name := cfg.String("git.author_name")
	email := cfg.String("git.author_email")
	cmd := fmt.Sprintf(
		"git config --global user.name %s && git config --global user.email %s && git config --global advice.detachedHead false",
		shellQuote(name), shellQuote(email))

	res, err := s.run(ctx, p.ID, sb, sandbox.Command{Cmd: cmd})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("configure git identity: exit %d", res.ExitCode)
	}

	return s.installGitCredential(ctx, p, sb)
}

// installGitCredential writes the PAT into the sandbox and points git at it.
//
// The token is written with sb.WriteFile rather than a shell command, and the
// only thing interpolated into a logged command is the file path. s.run streams
// every command and its output into project.setup_log, which is rendered on the
// Project screen — so a token in a command string would be a token on a web page
// and in Postgres forever.
//
// The credential lives beside the repository rather than in /tmp because it has
// to survive as long as the checkout does. Both are on the persistent sandbox
// filesystem, so a wake never loses one without the other, and a checkpoint
// restore rewinds them together.
func (s *Service) installGitCredential(ctx context.Context, p *store.Project, sb sandbox.Sandbox) error {
	s.step(ctx, p.ID, "git credential")

	if s.githubToken == "" {
		// Load refuses to boot without GITHUB_TOKEN, so this is unreachable in a
		// real process. It stays because a test harness can construct a Service
		// directly, and silently producing a sandbox that cannot push would be a
		// miserable thing to debug.
		s.logLine(ctx, p.ID, "warning: no GitHub token configured; clone and push will fail for private repositories")
		return nil
	}

	// x-access-token is GitHub's documented username for token authentication over
	// HTTPS. The password field is the PAT.
	line := fmt.Sprintf("https://x-access-token:%s@github.com\n", s.githubToken)
	if err := sb.WriteFile(ctx, GitCredentialsPath, []byte(line), 0o600); err != nil {
		return fmt.Errorf("write git credential: %w", err)
	}

	// insteadOf rewrites an SSH-style remote to HTTPS, so pasting
	// git@github.com:owner/repo into the project form still works. The sandbox has
	// no SSH key and never will.
	cmd := fmt.Sprintf(
		"git config --global credential.helper %s && git config --global url.%s.insteadOf %s",
		shellQuote("store --file="+sb.Path(GitCredentialsPath)),
		shellQuote("https://github.com/"), shellQuote("git@github.com:"))

	res, err := s.run(ctx, p.ID, sb, sandbox.Command{Cmd: cmd})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("configure git credential helper: exit %d", res.ExitCode)
	}
	s.logLine(ctx, p.ID, "note: GitHub PAT installed - clone, fetch, push, and pull requests all use native git over HTTPS")
	return nil
}

func (s *Service) cloneRepo(ctx context.Context, p *store.Project, sb sandbox.Sandbox) error {
	s.step(ctx, p.ID, "clone")

	// Remove any partial clone from an earlier failed attempt, so retrying is
	// not blocked by a non-empty target directory.
	repo := sb.Path(RepoPath)
	cmd := fmt.Sprintf("rm -rf %s && mkdir -p %s && git clone --progress %s %s",
		shellQuote(repo), shellQuote(sb.Path(WorkspacePath)), shellQuote(p.RepoURL), shellQuote(repo))

	res, err := s.run(ctx, p.ID, sb, sandbox.Command{Cmd: cmd, Timeout: 10 * time.Minute})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("clone failed with exit %d", res.ExitCode)
	}

	// git clone exits 0 even when it checked nothing out — a remote whose HEAD
	// points at a branch that does not exist only produces a warning. Without this
	// the project reaches "ready" with an empty working tree, and the first thing
	// the agent sees is a repository with no files and no explanation.
	head, err := s.run(ctx, p.ID, sb, sandbox.Command{Cmd: "git rev-parse --verify HEAD", Dir: RepoPath})
	if err != nil {
		return err
	}
	if head.ExitCode != 0 {
		return fmt.Errorf("clone produced no checkout: the remote's HEAD does not point at a branch that exists. " +
			"Check the repository's default branch")
	}
	return nil
}

// checkoutWorkBranch records the repository's real default branch and moves onto
// the doot branch.
func (s *Service) checkoutWorkBranch(ctx context.Context, p *store.Project, sb sandbox.Sandbox) error {
	s.step(ctx, p.ID, "branch")

	// Detected rather than asked for: whatever the clone landed on is the
	// repository's default branch, which is more reliable than a form field.
	res, err := s.run(ctx, p.ID, sb, sandbox.Command{
		Cmd: "git rev-parse --abbrev-ref HEAD",
		Dir: RepoPath,
	})
	if err != nil {
		return err
	}
	detected := strings.TrimSpace(res.Stdout)
	if res.ExitCode == 0 && detected != "" && detected != "HEAD" {
		if err := s.store.SetDefaultBranch(ctx, p.ID, detected); err != nil {
			return err
		}
		p.DefaultBranch = detected
		s.logLine(ctx, p.ID, "default branch: "+detected)
	}

	res, err = s.run(ctx, p.ID, sb, sandbox.Command{
		Cmd: "git checkout -B " + store.WorkBranch,
		Dir: RepoPath,
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("checkout %s failed with exit %d", store.WorkBranch, res.ExitCode)
	}
	return nil
}

// dependencyInstall picks one install command from the repository's lockfiles.
//
// An elif chain rather than a table in Go. The previous version was a nine-entry
// slice of marker/command pairs, a seenJS boolean so two JavaScript managers could
// not both run, and a `test -f` round trip into the sandbox per entry — nine execs
// before concluding there was nothing to install. Precedence is what an elif chain
// is for, and the shell can test for a file.
const dependencyInstall = `
if   [ -f pnpm-lock.yaml ];    then echo "pnpm-lock.yaml";    pnpm install --frozen-lockfile
elif [ -f yarn.lock ];         then echo "yarn.lock";         yarn install --frozen-lockfile
elif [ -f package-lock.json ]; then echo "package-lock.json"; npm ci
elif [ -f package.json ];      then echo "package.json";      npm install
elif [ -f go.mod ];            then echo "go.mod";            go mod download
elif [ -f uv.lock ];           then echo "uv.lock";           uv sync
elif [ -f requirements.txt ];  then echo "requirements.txt";  pip install -r requirements.txt
elif [ -f Gemfile ];           then echo "Gemfile";           bundle install
elif [ -f Cargo.toml ];        then echo "Cargo.toml";        cargo fetch
else echo "no recognised dependency manifest; nothing to install"
fi
`

// installDependencies runs whatever the repository's lockfiles imply.
//
// Failures are warnings. A missing toolchain or a flaky registry should not stop
// the project from being usable: the agent can install what it needs, and a project
// stuck in error because one install failed is less useful than a ready one with a
// warning in the log.
func (s *Service) installDependencies(ctx context.Context, p *store.Project, sb sandbox.Sandbox) {
	s.step(ctx, p.ID, "dependencies")

	res, err := s.run(ctx, p.ID, sb, sandbox.Command{
		Cmd:     dependencyInstall,
		Dir:     RepoPath,
		Timeout: 10 * time.Minute,
	})
	if err != nil {
		s.logLine(ctx, p.ID, "warning: "+err.Error())
		return
	}
	if res.ExitCode != 0 {
		s.logLine(ctx, p.ID, fmt.Sprintf("warning: dependency install exited %d; the agent can fix this later", res.ExitCode))
	}
}

// run executes a command and streams its output into the setup log.
func (s *Service) run(ctx context.Context, projectID string, sb sandbox.Sandbox, cmd sandbox.Command) (sandbox.ExecResult, error) {
	s.logLine(ctx, projectID, "$ "+cmd.Cmd)

	res, err := sb.Exec(ctx, cmd)
	if err != nil {
		return res, err
	}
	if out := strings.TrimRight(res.Output(), "\n"); out != "" {
		s.logLine(ctx, projectID, indent(out))
	}
	return res, nil
}

func (s *Service) step(ctx context.Context, projectID, title string) {
	s.logLine(ctx, projectID, "")
	s.logLine(ctx, projectID, "--- "+title+" ---")
}

func (s *Service) logLine(ctx context.Context, projectID, line string) {
	if err := s.store.AppendSetupLog(ctx, projectID, line); err != nil {
		s.log.Error("append setup log", "error", err, "project_id", projectID)
	}
}

func indent(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}
