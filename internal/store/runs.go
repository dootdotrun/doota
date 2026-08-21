package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Run states mirror the run.state database constraint.
const (
	RunIdle          = "idle"
	RunRunning       = "running"
	RunAwaitingHuman = "awaiting_human"
	RunPaused        = "paused"
	RunDone          = "done"
	RunFailed        = "failed"
)

// Awaiting reasons mirror the awaiting_reason database constraint.
const (
	AwaitingPlanApproval = "plan_approval"
	AwaitingQuestion     = "question"
	AwaitingError        = "error"
)

// Run is the durable state of one agent task.
//
// There is no lease. A previous version held a 45-second lease per run, renewed it
// from a goroutine, ran a reconciler ticker every five seconds forever, and drained
// to a durable boundary on shutdown so a replacement machine could take a run over
// mid-flight. That is correct engineering for a fleet. This is one operator, one
// project, one machine: at most one run can exist at a time, enforced by a partial
// unique index, and a run interrupted by a restart is picked up again at boot.
type Run struct {
	ID              string
	ProjectID       string
	State           string
	AwaitingReason  sql.NullString
	AwaitingPayload json.RawMessage
	PauseRequested  bool
	Error           sql.NullString
	StepCount       int
	StartedAt       time.Time
	UpdatedAt       time.Time
	FinishedAt      sql.NullTime
}

func (r *Run) Awaiting() string {
	if r.AwaitingReason.Valid {
		return r.AwaitingReason.String
	}
	return ""
}

func (r *Run) Active() bool {
	return r.State == RunRunning || r.State == RunAwaitingHuman || r.State == RunPaused
}

// ErrActiveRun prevents transcript transitions that would invalidate a live
// runner's model context.
var ErrActiveRun = errors.New("an active run prevents this operation")

// lockProjectTx serializes competing project-scoped transitions. The advisory lock
// exists only for this transaction and needs no schema object or cleanup.
func lockProjectTx(ctx context.Context, tx *sql.Tx, projectID string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, projectID); err != nil {
		return fmt.Errorf("lock project transition: %w", err)
	}
	return nil
}

const runColumns = `id, project_id, state, awaiting_reason, awaiting_payload,
	pause_requested, error, step_count, started_at, updated_at, finished_at`

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var payload []byte
	err := row.Scan(&r.ID, &r.ProjectID, &r.State, &r.AwaitingReason, &payload,
		&r.PauseRequested, &r.Error, &r.StepCount, &r.StartedAt, &r.UpdatedAt, &r.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan run: %w", err)
	}
	r.AwaitingPayload = json.RawMessage(payload)
	return &r, nil
}

// LatestRun returns the most recent run whatever its state, finished ones
// included.
//
// ActiveRun deliberately excludes idle and done, which is right for deciding
// whether work is in flight and wrong for showing what happened. With only
// ActiveRun to ask, the moment a run finished the Chat screen saw no run at all
// and printed the same "idle" chip a project that had never been used would
// print — so a completed task was indistinguishable from one never started. This
// is how the screen can say "finished" or "shipped" instead.
func (s *Store) LatestRun(ctx context.Context, projectID string) (*Run, error) {
	return scanRun(s.DB.QueryRowContext(ctx, `SELECT `+runColumns+` FROM run
		WHERE project_id = $1 ORDER BY started_at DESC LIMIT 1`, projectID))
}

// ActiveRun returns the sole resumable run for a project, if there is one.
func (s *Store) ActiveRun(ctx context.Context, projectID string) (*Run, error) {
	return scanRun(s.DB.QueryRowContext(ctx, `SELECT `+runColumns+` FROM run
		WHERE project_id = $1 AND state IN ($2, $3, $4) ORDER BY started_at DESC LIMIT 1`,
		projectID, RunRunning, RunAwaitingHuman, RunPaused))
}

func (s *Store) RunByID(ctx context.Context, id string) (*Run, error) {
	return scanRun(s.DB.QueryRowContext(ctx, `SELECT `+runColumns+` FROM run WHERE id = $1`, id))
}

// InterruptedRuns returns runs left mid-flight by a restart, for the single
// boot-time recovery pass.
func (s *Store) InterruptedRuns(ctx context.Context) ([]*Run, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+runColumns+` FROM run
		WHERE state = $1 ORDER BY started_at`, RunRunning)
	if err != nil {
		return nil, fmt.Errorf("list interrupted runs: %w", err)
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RequestPause records the operator's stop. One press: the model stream and the
// running tool are both cancelled, and the run parks at the last durable boundary.
//
// The previous two-stage escalation, where a first press cancelled the stream and a
// second cancelled the tool, made the operator press a button twice to find out
// which kind of stop they had asked for.
func (s *Store) RequestPause(ctx context.Context, projectID string) (*Run, error) {
	return scanRun(s.DB.QueryRowContext(ctx, `UPDATE run
		SET pause_requested = true, updated_at = now()
		WHERE project_id = $1 AND state = $2 RETURNING `+runColumns, projectID, RunRunning))
}

func (s *Store) MarkPaused(ctx context.Context, runID string) (*Run, error) {
	return scanRun(s.DB.QueryRowContext(ctx, `UPDATE run SET state = $2, updated_at = now()
		WHERE id = $1 AND state = $3 RETURNING `+runColumns, runID, RunPaused, RunRunning))
}

func (s *Store) ResumeRun(ctx context.Context, projectID string) (*Run, error) {
	return scanRun(s.DB.QueryRowContext(ctx, `UPDATE run
		SET state = $2, pause_requested = false,
			awaiting_reason = NULL, awaiting_payload = NULL, error = NULL, updated_at = now()
		WHERE project_id = $1 AND state IN ($3, $4)
		  AND NOT (state = $4 AND awaiting_reason = $5)
		RETURNING `+runColumns,
		projectID, RunRunning, RunPaused, RunAwaitingHuman, AwaitingPlanApproval))
}

func (s *Store) AwaitHuman(ctx context.Context, runID, reason string, payload any, cause string) (*Run, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode awaiting payload: %w", err)
	}
	return scanRun(s.DB.QueryRowContext(ctx, `UPDATE run
		SET state = $2, awaiting_reason = $3, awaiting_payload = $4, error = $5, updated_at = now()
		WHERE id = $1 AND state = $6 RETURNING `+runColumns,
		runID, RunAwaitingHuman, reason, encoded, nullable(cause), RunRunning))
}

func (s *Store) FinishRun(ctx context.Context, runID, state string, cause string) (*Run, error) {
	return scanRun(s.DB.QueryRowContext(ctx, `UPDATE run
		SET state = $2, error = $3, finished_at = now(), updated_at = now()
		WHERE id = $1 RETURNING `+runColumns, runID, state, nullable(cause)))
}

func (s *Store) IncrementRunStep(ctx context.Context, runID string) (*Run, error) {
	return scanRun(s.DB.QueryRowContext(ctx, `UPDATE run SET step_count = step_count + 1, updated_at = now()
		WHERE id = $1 RETURNING `+runColumns, runID))
}

// TrailingToolCalls returns unanswered calls in the most recent assistant tool-call
// message. It is the crash-recovery boundary: assistant intent is durable before a
// side effect begins, so a restarted runner replays only the missing results.
func (s *Store) TrailingToolCalls(ctx context.Context, runID string) (*Message, []string, error) {
	assistant, err := scanMessage(s.DB.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM message
		WHERE run_id = $1 AND role = $2 AND tool_calls IS NOT NULL
		ORDER BY id DESC LIMIT 1`, runID, RoleAssistant))
	if errors.Is(err, ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("trailing tool calls: %w", err)
	}
	var calls []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(assistant.ToolCalls, &calls); err != nil {
		return nil, nil, fmt.Errorf("decode trailing tool calls: %w", err)
	}
	var pending []string
	for _, call := range calls {
		var exists bool
		err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM message
			WHERE run_id = $1 AND role = $2 AND tool_call_id = $3)`, runID, RoleTool, call.ID).Scan(&exists)
		if err != nil {
			return nil, nil, fmt.Errorf("check tool result: %w", err)
		}
		if !exists {
			pending = append(pending, call.ID)
		}
	}
	return assistant, pending, nil
}

// CreateRunWithMessage makes the first persisted position of a run atomic: a
// restarted runner never observes a new run without the request that created it.
func (s *Store) CreateRunWithMessage(ctx context.Context, projectID, content string) (*Run, *Message, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin run creation: %w", err)
	}
	defer tx.Rollback()
	if err := lockProjectTx(ctx, tx, projectID); err != nil {
		return nil, nil, err
	}
	r, err := scanRun(tx.QueryRowContext(ctx, `INSERT INTO run (project_id, state) VALUES ($1, $2)
		RETURNING `+runColumns, projectID, RunRunning))
	if err != nil {
		return nil, nil, fmt.Errorf("create run: %w", err)
	}
	m, err := scanMessage(tx.QueryRowContext(ctx, `INSERT INTO message (project_id, run_id, role, content)
		VALUES ($1, $2, $3, $4) RETURNING `+messageColumns, projectID, r.ID, RoleUser, content))
	if err != nil {
		return nil, nil, fmt.Errorf("append initial message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit run creation: %w", err)
	}
	return r, m, nil
}

// AppendToolResultAndAwait commits the protocol-required tool result and the
// awaiting state together, so a restart cannot insert a human reply between an
// assistant call and its result.
func (s *Store) AppendToolResultAndAwait(ctx context.Context, in NewMessage, reason string, payload any) (*Message, *Run, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("encode awaiting payload: %w", err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin awaiting result: %w", err)
	}
	defer tx.Rollback()
	m, err := appendMessageTx(ctx, tx, in)
	if err != nil {
		return nil, nil, err
	}
	r, err := scanRun(tx.QueryRowContext(ctx, `UPDATE run
		SET state = $2, awaiting_reason = $3, awaiting_payload = $4, updated_at = now()
		WHERE id = $1 AND state = $5 RETURNING `+runColumns,
		in.RunID, RunAwaitingHuman, reason, encoded, RunRunning))
	if err != nil {
		return nil, nil, fmt.Errorf("await human: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit awaiting result: %w", err)
	}
	return m, r, nil
}

// FinishIfTerminalAssistant closes the narrow restart window where a final
// assistant message landed but the state transition did not.
func (s *Store) FinishIfTerminalAssistant(ctx context.Context, runID string) (*Run, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin terminal recovery: %w", err)
	}
	defer tx.Rollback()
	m, err := scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM message
		WHERE run_id = $1 ORDER BY id DESC LIMIT 1`, runID))
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read terminal message: %w", err)
	}
	if m.Role != RoleAssistant || m.HasToolCalls() || m.Interrupted {
		return nil, false, nil
	}
	r, err := scanRun(tx.QueryRowContext(ctx, `UPDATE run
		SET state = $2, finished_at = now(), updated_at = now()
		WHERE id = $1 AND state = $3 RETURNING `+runColumns, runID, RunIdle, RunRunning))
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("finish terminal recovery: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit terminal recovery: %w", err)
	}
	return r, true, nil
}

// AppendAnswerAndResume makes an awaiting-human reply a single durable boundary.
func (s *Store) AppendAnswerAndResume(ctx context.Context, projectID, runID, content string) (*Message, *Run, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin answer resume: %w", err)
	}
	defer tx.Rollback()
	m, err := scanMessage(tx.QueryRowContext(ctx, `INSERT INTO message (project_id, run_id, role, content)
		VALUES ($1, $2, $3, $4) RETURNING `+messageColumns, projectID, runID, RoleUser, content))
	if err != nil {
		return nil, nil, fmt.Errorf("append answer: %w", err)
	}
	r, err := scanRun(tx.QueryRowContext(ctx, `UPDATE run
		SET state = $2, pause_requested = false,
			awaiting_reason = NULL, awaiting_payload = NULL, error = NULL, updated_at = now()
		WHERE id = $1 AND project_id = $3 AND state = $4 RETURNING `+runColumns,
		runID, RunRunning, projectID, RunAwaitingHuman))
	if err != nil {
		return nil, nil, fmt.Errorf("resume answered run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit answer resume: %w", err)
	}
	return m, r, nil
}

// appendMessageTx inserts a transcript row inside an existing transaction.
func appendMessageTx(ctx context.Context, tx *sql.Tx, in NewMessage) (*Message, error) {
	m, err := scanMessage(tx.QueryRowContext(ctx, `INSERT INTO message
		(project_id, run_id, role, kind, content, reasoning, tool_calls, tool_call_id,
		 tool_name, token_count, interrupted, tool_display)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING `+messageColumns,
		in.ProjectID, nullable(in.RunID), in.Role, nullable(in.Kind), in.Content,
		nullable(in.Reasoning), nullableJSON(in.ToolCalls), nullable(in.ToolCallID),
		nullable(in.ToolName), nullableInt(in.TokenCount), in.Interrupted, nullableJSON(in.ToolDisplay)))
	if err != nil {
		return nil, fmt.Errorf("append message in transaction: %w", err)
	}
	return m, nil
}
