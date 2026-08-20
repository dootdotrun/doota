// Package project owns the single project and its sandbox lifecycle.
package project

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dootdotrun/doot-ai/internal/sandbox"
	"github.com/dootdotrun/doot-ai/internal/store"
)

// Paths inside the sandbox. Absolute and fixed, so nothing has to discover them.
const (
	WorkspacePath = "/workspace"
	RepoPath      = "/workspace/repo"
	SetupPath     = "/tmp/doot-setup.sh"
	LogDir        = "/tmp/doot-logs"

	// GitCredentialsPath holds the GitHub PAT in git's credential-store format.
	//
	// Beside the checkout, not in /tmp: it has to live exactly as long as the
	// repository it authenticates, and both are on the persistent filesystem.
	// Outside RepoPath so it can never be staged into a commit.
	GitCredentialsPath = "/workspace/.doot-git-credentials"
)

// provisionTimeout bounds a whole setup run. Cloning a large repo and installing
// dependencies can be slow; hanging forever is worse than failing.
const (
	provisionTimeout  = 20 * time.Minute
	sandboxAttempts   = 4
	maxSandboxBackoff = 8 * time.Second
)

// Service coordinates the project row and its sandbox.
type Service struct {
	store    *store.Store
	provider sandbox.Provider
	log      *slog.Logger

	// githubToken is installed into each sandbox during provisioning. Held here
	// rather than in app_config because it is a credential, and this codebase
	// keeps credentials in the environment where they cannot be rendered.
	githubToken string

	mu       sync.Mutex
	inflight map[string]struct{}
}

// New builds the service.
func New(st *store.Store, provider sandbox.Provider, githubToken string, log *slog.Logger) *Service {
	return &Service{
		store:       st,
		provider:    provider,
		githubToken: githubToken,
		log:         log,
		inflight:    map[string]struct{}{},
	}
}

// ProviderKind reports which sandbox implementation is in use.
func (s *Service) ProviderKind() string { return s.provider.Kind() }

// Active returns the current project, or store.ErrNotFound.
func (s *Service) Active(ctx context.Context) (*store.Project, error) {
	return s.store.ActiveProject(ctx)
}

// Create inserts the project and starts provisioning in the background.
//
// Provisioning is not awaited: cloning and installing takes minutes, and the
// phone should get a rendered page immediately and watch the status change.
func (s *Service) Create(ctx context.Context, name, repoURL string, previewPort int) (*store.Project, error) {
	name = strings.TrimSpace(name)
	repoURL = strings.TrimSpace(repoURL)
	if name == "" {
		return nil, errors.New("project name is required")
	}
	if repoURL == "" {
		return nil, errors.New("repository URL is required")
	}
	if previewPort <= 0 || previewPort > 65535 {
		return nil, errors.New("preview port must be between 1 and 65535")
	}

	// default_branch is a placeholder: the real one is detected after cloning,
	// which is more reliable than asking.
	p, err := s.store.CreateProject(ctx, name, repoURL, "main", previewPort)
	if err != nil {
		return nil, err
	}

	s.log.Info("project created", "project_id", p.ID, "name", name, "repo", repoURL)
	s.startProvision(p.ID)
	return p, nil
}

// Recreate destroys the sandbox and provisions a fresh one.
func (s *Service) Recreate(ctx context.Context, p *store.Project) error {
	if name := p.Sandbox(); name != "" {
		if err := s.provider.Delete(ctx, name); err != nil {
			s.log.Error("delete sandbox during recreate", "error", err, "sandbox", name)
			// Keep going: a sandbox we cannot delete should not block getting a
			// working one, and leaving the project stuck is the worse outcome.
		}
	}
	if err := s.store.ResetSetupLog(ctx, p.ID); err != nil {
		return err
	}
	// Whatever the agent had running died with the old sandbox.
	s.stopBackgroundProcesses(ctx, p)
	if err := s.store.SetSandboxStatus(ctx, p.ID, store.SandboxProvisioning); err != nil {
		return err
	}
	s.log.Info("recreating sandbox", "project_id", p.ID)
	s.startProvision(p.ID)
	return nil
}

// Delete destroys the sandbox and soft-deletes the project, keeping its history.
func (s *Service) Delete(ctx context.Context, p *store.Project) error {
	if name := p.Sandbox(); name != "" {
		if err := s.provider.Delete(ctx, name); err != nil {
			s.log.Error("delete sandbox", "error", err, "sandbox", name)
		}
	}
	s.stopBackgroundProcesses(ctx, p)
	if err := s.store.SoftDeleteProject(ctx, p.ID); err != nil {
		return err
	}
	s.log.Info("project deleted", "project_id", p.ID)
	return nil
}

// Sandbox returns a handle to the project's sandbox.
func (s *Service) Sandbox(ctx context.Context, p *store.Project) (sandbox.Sandbox, error) {
	name := p.Sandbox()
	if name == "" {
		return nil, fmt.Errorf("project has no sandbox")
	}
	return s.provider.Get(ctx, name)
}

// ReadySandbox obtains a usable sandbox for one agent operation. Provider reads,
// state checks, and waking are safe to retry because none can mutate the project
// repository. A tool itself is never replayed here: retrying a write after an
// ambiguous transport failure could duplicate its side effect.
func (s *Service) ReadySandbox(ctx context.Context, p *store.Project) (sandbox.Sandbox, error) {
	var sb sandbox.Sandbox
	if err := s.retrySandbox(ctx, "open sandbox", func() error {
		var err error
		sb, err = s.Sandbox(ctx, p)
		return err
	}); err != nil {
		s.markSandboxUnavailable(ctx, p, err)
		return nil, fmt.Errorf("open sandbox: %w", err)
	}

	var state sandbox.State
	if err := s.retrySandbox(ctx, "read sandbox state", func() error {
		var err error
		state, err = sb.State(ctx)
		return err
	}); err != nil {
		s.markSandboxUnavailable(ctx, p, err)
		return nil, fmt.Errorf("read sandbox state: %w", err)
	}
	if state == sandbox.StateMissing {
		s.markSandboxUnavailable(ctx, p, sandbox.ErrNotFound)
		return nil, sandbox.ErrNotFound
	}
	if state == sandbox.StateSleeping {
		if err := s.retrySandbox(ctx, "wake sandbox", func() error { return sb.Wake(ctx) }); err != nil {
			s.markSandboxUnavailable(ctx, p, err)
			return nil, fmt.Errorf("wake sandbox: %w", err)
		}
		s.setStatus(ctx, p, store.SandboxReady)
	}
	return sb, nil
}

func (s *Service) markSandboxUnavailable(ctx context.Context, p *store.Project, err error) {
	if errors.Is(err, sandbox.ErrNotFound) {
		s.setStatus(ctx, p, store.SandboxMissing)
	}
}

func (s *Service) retrySandbox(ctx context.Context, operation string, fn func() error) error {
	var last error
	for attempt := 0; attempt < sandboxAttempts; attempt++ {
		if err := fn(); err == nil {
			return nil
		} else if errors.Is(err, sandbox.ErrNotFound) || errors.Is(err, context.Canceled) {
			return err
		} else {
			last = err
		}
		if attempt == sandboxAttempts-1 {
			break
		}
		delay := time.Second << attempt
		if delay > maxSandboxBackoff {
			delay = maxSandboxBackoff
		}
		s.log.Warn("retry sandbox operation", "operation", operation, "attempt", attempt+1, "error", last, "retry_in", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return last
}

// Wake starts a sleeping sandbox and records the new status.
func (s *Service) Wake(ctx context.Context, p *store.Project) error {
	_, err := s.ReadySandbox(ctx, p)
	return err
}

// RefreshStatus reconciles the stored status with the provider.
func (s *Service) RefreshStatus(ctx context.Context, p *store.Project) {
	if p.IsProvisioning() || p.Sandbox() == "" {
		return
	}

	var sb sandbox.Sandbox
	err := s.retrySandbox(ctx, "refresh sandbox", func() error {
		var err error
		sb, err = s.provider.Get(ctx, p.Sandbox())
		return err
	})
	if errors.Is(err, sandbox.ErrNotFound) {
		s.setStatus(ctx, p, store.SandboxMissing)
		return
	}
	if err != nil {
		s.log.Error("refresh sandbox status", "error", err, "project_id", p.ID)
		return
	}

	var state sandbox.State
	err = s.retrySandbox(ctx, "read refreshed sandbox state", func() error {
		var err error
		state, err = sb.State(ctx)
		return err
	})
	if err != nil {
		s.log.Error("read sandbox state", "error", err, "project_id", p.ID)
		return
	}

	switch state {
	case sandbox.StateRunning:
		// Only promote to ready from a state that implies setup finished.
		if p.SandboxStatus == store.SandboxSleeping {
			s.setStatus(ctx, p, store.SandboxReady)
		}
	case sandbox.StateSleeping:
		if p.SandboxStatus == store.SandboxReady {
			s.setStatus(ctx, p, store.SandboxSleeping)
		}
	case sandbox.StateMissing:
		s.setStatus(ctx, p, store.SandboxMissing)
	}
}

func (s *Service) setStatus(ctx context.Context, p *store.Project, status string) {
	if p.SandboxStatus == status {
		return
	}
	if err := s.store.SetSandboxStatus(ctx, p.ID, status); err != nil {
		s.log.Error("set sandbox status", "error", err, "project_id", p.ID)
		return
	}
	p.SandboxStatus = status
}

// stopBackgroundProcesses retires the project's background process rows.
//
// Best-effort and never fatal: failing to update bookkeeping should not stop a
// recovery action the user asked for.
func (s *Service) stopBackgroundProcesses(ctx context.Context, p *store.Project) {
	n, err := s.store.StopAllBackgroundProcesses(ctx, p.ID)
	if err != nil {
		s.log.Warn("retire background processes", "error", err, "project_id", p.ID)
		return
	}
	if n > 0 {
		s.log.Info("retired background processes", "count", n, "project_id", p.ID)
	}
}

// SetPreviewPort changes which port previews target.
func (s *Service) SetPreviewPort(ctx context.Context, p *store.Project, port int) error {
	if port <= 0 || port > 65535 {
		return errors.New("preview port must be between 1 and 65535")
	}
	return s.store.SetPreviewPort(ctx, p.ID, port)
}

// startProvision runs setup in the background, at most once per project.
func (s *Service) startProvision(projectID string) {
	s.mu.Lock()
	if _, running := s.inflight[projectID]; running {
		s.mu.Unlock()
		s.log.Warn("provisioning already in flight", "project_id", projectID)
		return
	}
	s.inflight[projectID] = struct{}{}
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.inflight, projectID)
			s.mu.Unlock()
		}()

		// Detached from the request context, which is cancelled as soon as the
		// response is written.
		ctx, cancel := context.WithTimeout(context.Background(), provisionTimeout)
		defer cancel()

		if err := s.provision(ctx, projectID); err != nil {
			s.log.Error("provisioning failed", "error", err, "project_id", projectID)
			s.logLine(ctx, projectID, "")
			s.logLine(ctx, projectID, "FAILED: "+err.Error())
			if err := s.store.SetSandboxStatus(ctx, projectID, store.SandboxError); err != nil {
				s.log.Error("mark provisioning failed", "error", err, "project_id", projectID)
			}
			return
		}

		if err := s.store.SetSandboxStatus(ctx, projectID, store.SandboxReady); err != nil {
			s.log.Error("mark project ready", "error", err, "project_id", projectID)
			return
		}
		s.logLine(ctx, projectID, "")
		s.logLine(ctx, projectID, "Ready.")
		s.log.Info("project ready", "project_id", projectID)
	}()
}

// SyncGoalStart catches the doot branch up with the configured default branch
// before an approved goal can touch files. It never resolves conflicts: a failed
// rebase is aborted and reported to the human with the working tree preserved.
func (s *Service) SyncGoalStart(ctx context.Context, p *store.Project) error {
	sb, err := s.ReadySandbox(ctx, p)
	if err != nil {
		return fmt.Errorf("open sandbox for goal start: %w", err)
	}
	status, err := sb.Exec(ctx, sandbox.Command{Cmd: "git status --porcelain", Dir: RepoPath, Timeout: 2 * time.Minute})
	if err != nil {
		return fmt.Errorf("inspect worktree: %w", err)
	}
	if status.ExitCode != 0 {
		return fmt.Errorf("inspect worktree: git exited %d: %s", status.ExitCode, strings.TrimSpace(status.Output()))
	}
	if strings.TrimSpace(status.Output()) != "" {
		return errors.New("cannot start a goal with uncommitted changes on doot; commit or clean the worktree first")
	}
	branch, err := sb.Exec(ctx, sandbox.Command{Cmd: "git rev-parse --abbrev-ref HEAD", Dir: RepoPath, Timeout: time.Minute})
	if err != nil {
		return fmt.Errorf("read work branch: %w", err)
	}
	if branch.ExitCode != 0 || strings.TrimSpace(branch.Stdout) != store.WorkBranch {
		return fmt.Errorf("cannot start a goal: HEAD must be on %q", store.WorkBranch)
	}
	refspec := "refs/heads/" + p.DefaultBranch + ":refs/remotes/origin/" + p.DefaultBranch
	fetch, err := sb.Exec(ctx, sandbox.Command{Cmd: "git fetch origin " + projectShellQuote(refspec), Dir: RepoPath, Timeout: 5 * time.Minute})
	if err != nil {
		return fmt.Errorf("fetch default branch: %w", err)
	}
	if fetch.ExitCode != 0 {
		return fmt.Errorf("fetch default branch failed (exit %d): %s", fetch.ExitCode, strings.TrimSpace(fetch.Output()))
	}
	rebase, err := sb.Exec(ctx, sandbox.Command{Cmd: "git rebase origin/" + projectShellQuote(p.DefaultBranch), Dir: RepoPath, Timeout: 5 * time.Minute})
	if err != nil {
		return fmt.Errorf("rebase doot: %w", err)
	}
	if rebase.ExitCode != 0 {
		abort, abortErr := sb.Exec(ctx, sandbox.Command{Cmd: "git rebase --abort", Dir: RepoPath, Timeout: 2 * time.Minute})
		if abortErr != nil || abort.ExitCode != 0 {
			return fmt.Errorf("rebase onto origin/%s failed and could not be aborted safely: %s", p.DefaultBranch, strings.TrimSpace(rebase.Output()))
		}
		return fmt.Errorf("rebase onto origin/%s conflicted and was aborted; resolve the branch manually, then resume: %s", p.DefaultBranch, strings.TrimSpace(rebase.Output()))
	}
	return nil
}

func projectShellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
