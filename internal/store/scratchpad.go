package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Task statuses. A task is a line on a board, not a contract.
const (
	TaskPending = "pending"
	TaskDoing   = "doing"
	TaskDone    = "done"
	TaskBlocked = "blocked"
)

// Plan statuses.
const (
	PlanDraft    = "draft"
	PlanApproved = "approved"
)

// Task is one line of the board: a one-line summary and a status.
//
// Deliberately not a contract. The previous design made every task carry an
// intent, a deliverable, and a stored verification string, then enforced ordering,
// a mandatory review id, and a commit match in Postgres before it could be
// completed. That bureaucracy protected the operator from an agent they are one
// tap away from stopping. A summary and a status is what a board needs.
type Task struct {
	N       int    `json:"n"`
	Summary string `json:"summary"`
	Status  string `json:"status"`
	Note    string `json:"note,omitempty"`
}

// Scratchpad is the whole working state of a plan, stored as one JSONB column.
//
// One column rather than goal and phase tables: it is read and written as a unit,
// only ever by one runner, and it is small. A table per level bought referential
// integrity for data that has exactly one owner.
type Scratchpad struct {
	Title      string `json:"title,omitempty"`
	Status     string `json:"status,omitempty"`
	BaseCommit string `json:"base_commit,omitempty"`
	Feedback   string `json:"feedback,omitempty"`
	Tasks      []Task `json:"tasks,omitempty"`
}

// Empty reports that there is no plan at all.
func (s Scratchpad) Empty() bool { return s.Title == "" && len(s.Tasks) == 0 }

// Approved reports that the operator released the plan for work.
func (s Scratchpad) Approved() bool { return s.Status == PlanApproved }

// AwaitingApproval reports a drafted plan that has not been approved yet.
func (s Scratchpad) AwaitingApproval() bool { return !s.Empty() && s.Status == PlanDraft }

// Current returns the task being worked, or the first not-yet-done one.
func (s Scratchpad) Current() *Task {
	for i := range s.Tasks {
		if s.Tasks[i].Status == TaskDoing {
			return &s.Tasks[i]
		}
	}
	for i := range s.Tasks {
		if s.Tasks[i].Status == TaskPending || s.Tasks[i].Status == TaskBlocked {
			return &s.Tasks[i]
		}
	}
	return nil
}

// AllDone reports that every task on an approved board is complete.
func (s Scratchpad) AllDone() bool {
	if s.Empty() {
		return false
	}
	for _, t := range s.Tasks {
		if t.Status != TaskDone {
			return false
		}
	}
	return true
}

// Render is what the model sees. The board goes into the system prompt on every
// call, so the agent never spends a tool call asking where it is.
func (s Scratchpad) Render() string {
	if s.Empty() {
		return "The task board is empty. No plan has been made."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Plan: %s (%s)\n", s.Title, s.Status)
	for _, t := range s.Tasks {
		fmt.Fprintf(&b, "%d. [%s] %s", t.N, t.Status, t.Summary)
		if t.Note != "" {
			fmt.Fprintf(&b, " — %s", t.Note)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// Scratchpad reads the board.
func (s *Store) Scratchpad(ctx context.Context, projectID string) (Scratchpad, error) {
	var raw []byte
	err := s.DB.QueryRowContext(ctx, `SELECT scratchpad FROM project WHERE id = $1`, projectID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Scratchpad{}, ErrNotFound
	}
	if err != nil {
		return Scratchpad{}, fmt.Errorf("read scratchpad: %w", err)
	}
	return decodeScratchpad(raw)
}

func decodeScratchpad(raw []byte) (Scratchpad, error) {
	var pad Scratchpad
	if len(raw) == 0 {
		return pad, nil
	}
	if err := json.Unmarshal(raw, &pad); err != nil {
		return Scratchpad{}, fmt.Errorf("decode scratchpad: %w", err)
	}
	return pad, nil
}

func (s *Store) writeScratchpad(ctx context.Context, tx *sql.Tx, projectID string, pad Scratchpad) error {
	encoded, err := json.Marshal(pad)
	if err != nil {
		return fmt.Errorf("encode scratchpad: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE project SET scratchpad = $2, updated_at = now() WHERE id = $1`, projectID, encoded); err != nil {
		return fmt.Errorf("write scratchpad: %w", err)
	}
	return nil
}

// scratchpadTx locks the project row and returns the current board, so a
// read-modify-write cannot interleave with another one.
func (s *Store) scratchpadTx(ctx context.Context, tx *sql.Tx, projectID string) (Scratchpad, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT scratchpad FROM project WHERE id = $1 FOR UPDATE`, projectID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Scratchpad{}, ErrNotFound
	}
	if err != nil {
		return Scratchpad{}, fmt.Errorf("lock scratchpad: %w", err)
	}
	return decodeScratchpad(raw)
}

// WritePlan replaces the board with a fresh draft and parks the run for approval,
// in one transaction so a restart never shows a plan without its approval gate.
func (s *Store) WritePlan(ctx context.Context, projectID, runID, title string, summaries []string, tool NewMessage) (Scratchpad, *Run, *Message, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Scratchpad{}, nil, nil, errors.New("a plan needs a title")
	}
	if len(summaries) == 0 {
		return Scratchpad{}, nil, nil, errors.New("a plan needs at least one task")
	}
	pad := Scratchpad{Title: title, Status: PlanDraft}
	for i, summary := range summaries {
		summary = strings.TrimSpace(summary)
		if summary == "" {
			return Scratchpad{}, nil, nil, fmt.Errorf("task %d needs a summary", i+1)
		}
		pad.Tasks = append(pad.Tasks, Task{N: i + 1, Summary: summary, Status: TaskPending})
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Scratchpad{}, nil, nil, err
	}
	defer tx.Rollback()
	if _, err := s.scratchpadTx(ctx, tx, projectID); err != nil {
		return Scratchpad{}, nil, nil, err
	}
	if err := s.writeScratchpad(ctx, tx, projectID, pad); err != nil {
		return Scratchpad{}, nil, nil, err
	}
	m, err := appendMessageTx(ctx, tx, tool)
	if err != nil {
		return Scratchpad{}, nil, nil, err
	}
	payload, _ := json.Marshal(map[string]any{"title": pad.Title, "tasks": len(pad.Tasks)})
	r, err := scanRun(tx.QueryRowContext(ctx, `UPDATE run
		SET state = $2, awaiting_reason = $3, awaiting_payload = $4, updated_at = now()
		WHERE id = $1 AND state = $5 RETURNING `+runColumns,
		runID, RunAwaitingHuman, AwaitingPlanApproval, payload, RunRunning))
	if err != nil {
		return Scratchpad{}, nil, nil, fmt.Errorf("await plan approval: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Scratchpad{}, nil, nil, fmt.Errorf("commit plan: %w", err)
	}
	return pad, r, m, nil
}

// ApprovePlan releases the board for work and records the commit the diff will be
// measured from, so the reviewer can be shown exactly this plan's changes.
//
// The approval is also appended as a user turn. This is not decoration: the model
// has just told the operator it is waiting, and the last thing in its context is a
// tool result saying the plan awaits approval. A status field on the board is too
// quiet to overturn that — without a turn saying "approved, begin", the model reads
// its own transcript and correctly concludes it is still waiting. Rejection already
// worked this way; approval was the asymmetry.
func (s *Store) ApprovePlan(ctx context.Context, projectID, baseCommit string) (Scratchpad, *Run, *Message, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Scratchpad{}, nil, nil, err
	}
	defer tx.Rollback()
	pad, err := s.scratchpadTx(ctx, tx, projectID)
	if err != nil {
		return Scratchpad{}, nil, nil, err
	}
	if !pad.AwaitingApproval() {
		return Scratchpad{}, nil, nil, errors.New("there is no drafted plan waiting for approval")
	}
	pad.Status = PlanApproved
	pad.BaseCommit = strings.TrimSpace(baseCommit)
	pad.Feedback = ""
	if err := s.writeScratchpad(ctx, tx, projectID, pad); err != nil {
		return Scratchpad{}, nil, nil, err
	}
	r, err := scanRun(tx.QueryRowContext(ctx, `UPDATE run
		SET state = $2, awaiting_reason = NULL, awaiting_payload = NULL, updated_at = now()
		WHERE project_id = $1 AND state = $3 AND awaiting_reason = $4 RETURNING `+runColumns,
		projectID, RunRunning, RunAwaitingHuman, AwaitingPlanApproval))
	if err != nil {
		return Scratchpad{}, nil, nil, fmt.Errorf("resume approved run: %w", err)
	}
	m, err := appendMessageTx(ctx, tx, NewMessage{ProjectID: projectID, RunID: r.ID, Role: RoleUser,
		Content: "Plan approved. Start working through the subtasks now: mark each one doing, do the work, " +
			"commit it, mark it done. Do not ask again for permission."})
	if err != nil {
		return Scratchpad{}, nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return Scratchpad{}, nil, nil, fmt.Errorf("commit approval: %w", err)
	}
	return pad, r, m, nil
}

// RevisePlan sends the board back with feedback and resumes the run so the model
// can present a new one. The feedback also lands in the transcript as a user turn,
// because that is where the model actually reads it.
func (s *Store) RevisePlan(ctx context.Context, projectID, feedback string) (*Run, *Message, error) {
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		return nil, nil, errors.New("revision feedback is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	pad, err := s.scratchpadTx(ctx, tx, projectID)
	if err != nil {
		return nil, nil, err
	}
	if !pad.AwaitingApproval() {
		return nil, nil, errors.New("there is no drafted plan waiting for approval")
	}
	if err := s.writeScratchpad(ctx, tx, projectID, Scratchpad{Feedback: feedback}); err != nil {
		return nil, nil, err
	}
	r, err := scanRun(tx.QueryRowContext(ctx, `UPDATE run
		SET state = $2, awaiting_reason = NULL, awaiting_payload = NULL, updated_at = now()
		WHERE project_id = $1 AND state = $3 AND awaiting_reason = $4 RETURNING `+runColumns,
		projectID, RunRunning, RunAwaitingHuman, AwaitingPlanApproval))
	if err != nil {
		return nil, nil, fmt.Errorf("resume revised run: %w", err)
	}
	m, err := appendMessageTx(ctx, tx, NewMessage{ProjectID: projectID, RunID: r.ID, Role: RoleUser,
		Content: "Revise the plan: " + feedback + "\n\nPresent the new plan with create_plan."})
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit revision: %w", err)
	}
	return r, m, nil
}

// UpdateTask moves one line on the board.
//
// No ordering rule and no completion gate. If the model marks task 3 done before
// task 2, that is visible on the board and the operator can say so; enforcing it
// in Postgres cost more than it ever caught.
func (s *Store) UpdateTask(ctx context.Context, projectID string, n int, status, note string, tool NewMessage) (Scratchpad, *Message, error) {
	switch status {
	case TaskPending, TaskDoing, TaskDone, TaskBlocked:
	default:
		return Scratchpad{}, nil, fmt.Errorf("status must be pending, doing, done, or blocked")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Scratchpad{}, nil, err
	}
	defer tx.Rollback()
	pad, err := s.scratchpadTx(ctx, tx, projectID)
	if err != nil {
		return Scratchpad{}, nil, err
	}
	if pad.Empty() {
		return Scratchpad{}, nil, errors.New("there is no plan to update")
	}
	found := false
	for i := range pad.Tasks {
		if pad.Tasks[i].N == n {
			pad.Tasks[i].Status = status
			pad.Tasks[i].Note = strings.TrimSpace(note)
			found = true
			break
		}
	}
	if !found {
		return Scratchpad{}, nil, fmt.Errorf("there is no task %d on the board", n)
	}
	if err := s.writeScratchpad(ctx, tx, projectID, pad); err != nil {
		return Scratchpad{}, nil, err
	}
	m, err := appendMessageTx(ctx, tx, tool)
	if err != nil {
		return Scratchpad{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return Scratchpad{}, nil, fmt.Errorf("commit task update: %w", err)
	}
	return pad, m, nil
}

// ClearPlan empties the board after shipping.
func (s *Store) ClearPlan(ctx context.Context, projectID string) error {
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE project SET scratchpad = '{}', updated_at = now() WHERE id = $1`, projectID); err != nil {
		return fmt.Errorf("clear scratchpad: %w", err)
	}
	return nil
}

// Memories is the operator's durable preferences: conventions, tastes, decisions
// worth surviving a cleared conversation.
//
// One text column, injected into the system prompt. Not a table, not embeddings,
// not retrieval — a single-operator tool accumulates a page of preferences, not a
// corpus, and a page fits in every request.
func (s *Store) Memories(ctx context.Context, projectID string) (string, error) {
	var memories string
	err := s.DB.QueryRowContext(ctx, `SELECT memories FROM project WHERE id = $1`, projectID).Scan(&memories)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read memories: %w", err)
	}
	return memories, nil
}

// SetMemories replaces the memories column.
//
// Replace rather than append: the model is given the current text and asked to
// return the version it wants, so it can correct and prune instead of only ever
// growing a file it will later have to read past.
func (s *Store) SetMemories(ctx context.Context, projectID, memories string) error {
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE project SET memories = $2, updated_at = now() WHERE id = $1`,
		projectID, strings.TrimSpace(memories)); err != nil {
		return fmt.Errorf("write memories: %w", err)
	}
	return nil
}
