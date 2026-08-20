package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Background process status values, mirroring the check constraint.
const (
	BgRunning = "running"
	BgStopped = "stopped"
	BgUnknown = "unknown"
)

// BackgroundProcess is a dev server or watcher the agent started with bash_bg.
//
// Persisted rather than held in memory because the sandbox outlives this
// process: a redeploy replaces the Fly machine but leaves the Sprite and
// everything running inside it untouched. Without a row, the agent would come
// back with no idea what it had started.
type BackgroundProcess struct {
	ID        string
	ProjectID string
	Name      string
	Command   string
	Cwd       sql.NullString
	LogPath   string
	Status    string
	StartedAt time.Time
	StoppedAt sql.NullTime
}

// Dir returns the working directory, or "" when none was recorded.
func (b *BackgroundProcess) Dir() string {
	if b.Cwd.Valid {
		return b.Cwd.String
	}
	return ""
}

// IsRunning reports whether we believe the process is still alive.
func (b *BackgroundProcess) IsRunning() bool { return b.Status == BgRunning }

const backgroundColumns = `id, project_id, name, command, cwd, log_path, status,
	started_at, stopped_at`

func scanBackground(row interface{ Scan(...any) error }) (*BackgroundProcess, error) {
	var b BackgroundProcess
	err := row.Scan(&b.ID, &b.ProjectID, &b.Name, &b.Command, &b.Cwd, &b.LogPath,
		&b.Status, &b.StartedAt, &b.StoppedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan background process: %w", err)
	}
	return &b, nil
}

// StartBackgroundProcess records a newly started process, retiring any earlier
// one with the same name.
//
// Both statements run in one transaction because a partial unique index enforces
// one running process per name. Inserting first would hit a 23505 that says
// nothing useful; retiring first in the same transaction makes "starting a second
// process with an existing name replaces the first" true at the storage layer
// rather than only in the tool that calls it.
//
// Killing the old operating-system process is the caller's job. This only moves
// the bookkeeping, and the tool does them in the right order.
func (s *Store) StartBackgroundProcess(ctx context.Context, projectID, name, command, cwd, logPath string) (*BackgroundProcess, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin background process: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE background_process
		   SET status = $3, stopped_at = now()
		 WHERE project_id = $1 AND name = $2 AND status = $4`,
		projectID, name, BgStopped, BgRunning); err != nil {
		return nil, fmt.Errorf("retire previous background process: %w", err)
	}

	var dir any
	if cwd != "" {
		dir = cwd
	}

	b, err := scanBackground(tx.QueryRowContext(ctx, `
		INSERT INTO background_process (project_id, name, command, cwd, log_path)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+backgroundColumns, projectID, name, command, dir, logPath))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit background process: %w", err)
	}
	return b, nil
}

// RunningBackgroundProcess returns the live process with this name, or
// ErrNotFound.
func (s *Store) RunningBackgroundProcess(ctx context.Context, projectID, name string) (*BackgroundProcess, error) {
	return scanBackground(s.DB.QueryRowContext(ctx,
		`SELECT `+backgroundColumns+` FROM background_process
		  WHERE project_id = $1 AND name = $2 AND status = $3`, projectID, name, BgRunning))
}

// LatestBackgroundProcess returns the most recently started process with this
// name whatever its status.
//
// read_logs uses this rather than the running-only lookup: the log of a process
// that has since died is exactly what the agent needs to find out why.
func (s *Store) LatestBackgroundProcess(ctx context.Context, projectID, name string) (*BackgroundProcess, error) {
	return scanBackground(s.DB.QueryRowContext(ctx,
		`SELECT `+backgroundColumns+` FROM background_process
		  WHERE project_id = $1 AND name = $2
		  ORDER BY started_at DESC LIMIT 1`, projectID, name))
}

// ListBackgroundProcesses returns the project's processes, running ones first.
func (s *Store) ListBackgroundProcesses(ctx context.Context, projectID string) ([]*BackgroundProcess, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+backgroundColumns+` FROM background_process
		  WHERE project_id = $1
		  ORDER BY (status = $2) DESC, started_at DESC`, projectID, BgRunning)
	if err != nil {
		return nil, fmt.Errorf("list background processes: %w", err)
	}
	defer rows.Close()

	var out []*BackgroundProcess
	for rows.Next() {
		b, err := scanBackground(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate background processes: %w", err)
	}
	return out, nil
}

// RunningBackgroundNames lists the names of live processes, for error messages
// that tell the agent what it could have asked for.
func (s *Store) RunningBackgroundNames(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT name FROM background_process
		  WHERE project_id = $1 AND status = $2 ORDER BY name`, projectID, BgRunning)
	if err != nil {
		return nil, fmt.Errorf("list background names: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan background name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// SetBackgroundProcessStatus records that a process is no longer running.
func (s *Store) SetBackgroundProcessStatus(ctx context.Context, id, status string) error {
	var stoppedAt any
	if status != BgRunning {
		stoppedAt = time.Now().UTC()
	}
	res, err := s.DB.ExecContext(ctx, `
		UPDATE background_process SET status = $2, stopped_at = $3 WHERE id = $1`,
		id, status, stoppedAt)
	if err != nil {
		return fmt.Errorf("set background process status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// StopAllBackgroundProcesses marks every live process for a project as stopped.
//
// Called when the sandbox filesystem is rewound or replaced. Restoring a
// checkpoint or recreating the sandbox kills whatever was running, so leaving
// rows claiming otherwise would make read_logs report a dev server that no longer
// exists — a lie the agent would waste a turn discovering.
func (s *Store) StopAllBackgroundProcesses(ctx context.Context, projectID string) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE background_process
		   SET status = $2, stopped_at = now()
		 WHERE project_id = $1 AND status = $3`, projectID, BgStopped, BgRunning)
	if err != nil {
		return 0, fmt.Errorf("stop background processes: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
