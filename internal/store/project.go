package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Sandbox status values, mirroring the project.sandbox_status check constraint.
const (
	SandboxProvisioning = "provisioning"
	SandboxReady        = "ready"
	SandboxSleeping     = "sleeping"
	SandboxError        = "error"
	SandboxMissing      = "missing"
)

// ErrProjectExists is returned when a project already exists.
//
// This surfaces the database's own refusal rather than a pre-flight check in
// application code: the partial unique index is the rule, and a check-then-insert
// would just be a racier restatement of it.
var ErrProjectExists = errors.New("a project already exists")

// WorkBranch is the only branch this system ever works on.
const WorkBranch = "doot"

// Project is the single active project.
type Project struct {
	ID            string
	Name          string
	RepoURL       string
	DefaultBranch string
	WorkBranch    string
	SandboxID     sql.NullString
	SandboxStatus string
	PreviewPort   int
	SetupLog      sql.NullString
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     sql.NullTime
}

// Sandbox returns the sandbox name, or "" when none has been provisioned.
func (p *Project) Sandbox() string {
	if p.SandboxID.Valid {
		return p.SandboxID.String
	}
	return ""
}

// Log returns the captured setup output.
func (p *Project) Log() string {
	if p.SetupLog.Valid {
		return p.SetupLog.String
	}
	return ""
}

// IsProvisioning reports whether setup is still running.
func (p *Project) IsProvisioning() bool { return p.SandboxStatus == SandboxProvisioning }

// IsReady reports whether the project can be worked on.
func (p *Project) IsReady() bool { return p.SandboxStatus == SandboxReady }

const projectColumns = `id, name, repo_url, default_branch, work_branch, sandbox_id,
	sandbox_status, preview_port, setup_log, created_at, updated_at, deleted_at`

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.RepoURL, &p.DefaultBranch, &p.WorkBranch,
		&p.SandboxID, &p.SandboxStatus, &p.PreviewPort, &p.SetupLog,
		&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan project: %w", err)
	}
	return &p, nil
}

// CreateProject inserts the project, relying on the singleton index to reject a
// second one.
func (s *Store) CreateProject(ctx context.Context, name, repoURL, defaultBranch string, previewPort int) (*Project, error) {
	p, err := scanProject(s.DB.QueryRowContext(ctx, `
		INSERT INTO project (name, repo_url, default_branch, preview_port)
		VALUES ($1, $2, $3, $4)
		RETURNING `+projectColumns, name, repoURL, defaultBranch, previewPort))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrProjectExists
		}
		return nil, err
	}
	return p, nil
}

// ActiveProject returns the current project, or ErrNotFound.
func (s *Store) ActiveProject(ctx context.Context) (*Project, error) {
	return scanProject(s.DB.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM project WHERE deleted_at IS NULL`))
}

// ProjectByID returns a project regardless of deletion state.
func (s *Store) ProjectByID(ctx context.Context, id string) (*Project, error) {
	return scanProject(s.DB.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM project WHERE id = $1`, id))
}

// SetSandbox records the provisioned sandbox name.
//
// Called immediately after the sandbox is created and before any further setup:
// a sandbox with no database record is an orphan nobody will ever clean up.
func (s *Store) SetSandbox(ctx context.Context, id, sandboxName string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE project SET sandbox_id = $2, updated_at = now() WHERE id = $1`, id, sandboxName)
	if err != nil {
		return fmt.Errorf("set sandbox: %w", err)
	}
	return nil
}

// SetSandboxStatus updates the lifecycle status.
func (s *Store) SetSandboxStatus(ctx context.Context, id, status string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE project SET sandbox_status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("set sandbox status: %w", err)
	}
	return nil
}

// SetDefaultBranch records the repository's real default branch, detected after
// cloning rather than asked for on the create form.
func (s *Store) SetDefaultBranch(ctx context.Context, id, branch string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE project SET default_branch = $2, updated_at = now() WHERE id = $1`, id, branch)
	if err != nil {
		return fmt.Errorf("set default branch: %w", err)
	}
	return nil
}

// SetPreviewPort records which port previews should target.
func (s *Store) SetPreviewPort(ctx context.Context, id string, port int) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE project SET preview_port = $2, updated_at = now() WHERE id = $1`, id, port)
	if err != nil {
		return fmt.Errorf("set preview port: %w", err)
	}
	return nil
}

// ResetSetupLog clears the log before a fresh provisioning attempt.
func (s *Store) ResetSetupLog(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE project SET setup_log = '', updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("reset setup log: %w", err)
	}
	return nil
}

// AppendSetupLog adds a line to the setup log.
//
// Appending in the database rather than buffering in memory means the log is
// visible while provisioning is still running, and survives a restart that
// interrupts it.
func (s *Store) AppendSetupLog(ctx context.Context, id, line string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE project
		   SET setup_log = coalesce(setup_log, '') || $2,
		       updated_at = now()
		 WHERE id = $1`, id, line+"\n")
	if err != nil {
		return fmt.Errorf("append setup log: %w", err)
	}
	return nil
}

// SoftDeleteProject marks the project deleted, keeping every associated row.
func (s *Store) SoftDeleteProject(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE project
		   SET deleted_at = now(), sandbox_id = NULL, sandbox_status = $2, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`, id, SandboxMissing)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// FailInterruptedProvisioning marks projects left mid-setup as failed.
//
// Provisioning is not resumable yet: the durable runner arrives in Phase 5, and
// until then a restart mid-setup leaves a half-built sandbox. Better to say so
// and offer Recreate than to report a readiness that was never reached.
func (s *Store) FailInterruptedProvisioning(ctx context.Context) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE project
		   SET sandbox_status = $1,
		       setup_log = coalesce(setup_log, '') ||
		           E'\n[interrupted: the server restarted during setup. Use Recreate.]\n',
		       updated_at = now()
		 WHERE deleted_at IS NULL AND sandbox_status = $2`, SandboxError, SandboxProvisioning)
	if err != nil {
		return 0, fmt.Errorf("fail interrupted provisioning: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
