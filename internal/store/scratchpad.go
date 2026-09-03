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
//
// Named PlanStatus* rather than Plan*, because PlanDraft is now the type holding a
// proposed plan and having a constant and a type differ only by context is how you
// get a compile error nobody can read.
const (
	PlanStatusDraft    = "draft"
	PlanStatusApproved = "approved"
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

// Spec is the reasoning behind a plan, written before any of it is built.
//
// It exists because the operator is not a programmer and cannot review a list of
// subtask titles for soundness. A restated problem, an approach in plain language,
// and an explicit account of how each claim will be checked are reviewable by
// someone who cannot read the diff — which is the only review that actually happens
// here.
//
// It is a document, not a contract. Nothing in Postgres enforces that verification
// was performed or that an edge case was handled, and that is deliberate: an earlier
// design made every task carry a stored verification string and a matching commit,
// gated completion on them, and produced six failure modes whose only effect was to
// tell the agent its own work was illegal. Writing the spec down where the operator
// can read it does the useful part of that without the machinery.
type Spec struct {
	// Problem is the goal restated in the agent's own words, which is how the
	// operator finds out they were misunderstood before the work happens rather
	// than after.
	Problem string `json:"problem,omitempty"`

	// Approach is how, in language a non-programmer can follow.
	Approach string `json:"approach,omitempty"`

	// Verification is how each claim will actually be checked. Named commands and
	// observations, not "test it".
	Verification []string `json:"verification,omitempty"`

	// EdgeCases is what could break that the request did not mention. This is the
	// thinking the operator is least able to do for themselves.
	EdgeCases []string `json:"edge_cases,omitempty"`

	// Risks is what might go wrong, and anything the agent is unsure of.
	Risks string `json:"risks,omitempty"`

	// Questions is anything genuinely ambiguous, surfaced at approval time rather
	// than discovered halfway through. Not a blocker: the operator can approve
	// anyway and the agent proceeds on its stated assumption.
	Questions []string `json:"questions,omitempty"`
}

// Empty reports a spec with nothing in it.
func (s Spec) Empty() bool {
	return s.Problem == "" && s.Approach == "" && len(s.Verification) == 0 &&
		len(s.EdgeCases) == 0 && s.Risks == "" && len(s.Questions) == 0
}

// Scratchpad is the whole working state of a plan, stored as one JSONB column.
//
// One column rather than nested tables: it is read and written as a unit, only ever
// by one runner, and it is small. A table per level bought referential integrity for
// data that has exactly one owner.
type Scratchpad struct {
	Title      string `json:"title,omitempty"`
	Status     string `json:"status,omitempty"`
	BaseCommit string `json:"base_commit,omitempty"`
	Feedback   string `json:"feedback,omitempty"`
	Spec       Spec   `json:"spec,omitzero"`
	Tasks      []Task `json:"tasks,omitempty"`
}

// PlanDraft is a proposed plan: the spec, the title, and the subtasks.
//
// A struct rather than more positional arguments to WritePlan, which was already
// taking a title, a slice, and a message and would now take a spec as well.
type PlanDraft struct {
	Title string
	Tasks []string
	Spec  Spec
}

// Empty reports that there is no plan at all.
func (s Scratchpad) Empty() bool { return s.Title == "" && len(s.Tasks) == 0 }

// Approved reports that the operator released the plan for work.
func (s Scratchpad) Approved() bool { return s.Status == PlanStatusApproved }

// AwaitingApproval reports a drafted plan that has not been approved yet.
func (s Scratchpad) AwaitingApproval() bool { return !s.Empty() && s.Status == PlanStatusDraft }

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

	// The spec is shown back to the model on every turn alongside the tasks. It is
	// what it committed to: the approach it said it would take and, more importantly,
	// the verification it said it would perform. Keeping it in view is what makes
	// "verify before you claim" checkable against something specific rather than a
	// general instruction it can drift away from over twenty turns.
	if !s.Spec.Empty() {
		b.WriteString("\nAgreed spec:\n")
		if s.Spec.Problem != "" {
			fmt.Fprintf(&b, "  Problem: %s\n", s.Spec.Problem)
		}
		if s.Spec.Approach != "" {
			fmt.Fprintf(&b, "  Approach: %s\n", s.Spec.Approach)
		}
		for _, v := range s.Spec.Verification {
			fmt.Fprintf(&b, "  Must verify: %s\n", v)
		}
		for _, e := range s.Spec.EdgeCases {
			fmt.Fprintf(&b, "  Edge case: %s\n", e)
		}
		if s.Spec.Risks != "" {
			fmt.Fprintf(&b, "  Risks: %s\n", s.Spec.Risks)
		}
		b.WriteByte('\n')
	}

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
func (s *Store) WritePlan(ctx context.Context, projectID, runID string, draft PlanDraft, tool NewMessage) (Scratchpad, *Run, *Message, error) {
	title := strings.TrimSpace(draft.Title)
	if title == "" {
		return Scratchpad{}, nil, nil, errors.New("a plan needs a title")
	}
	if len(draft.Tasks) == 0 {
		return Scratchpad{}, nil, nil, errors.New("a plan needs at least one task")
	}
	pad := Scratchpad{Title: title, Status: PlanStatusDraft, Spec: draft.Spec}
	for i, summary := range draft.Tasks {
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
	pad.Status = PlanStatusApproved
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

// Orientation is what the agent worked out about this repository for itself: how to
// build it, how to test it, where things are, what the local idioms are.
//
// Separate from Memories, which holds what the operator said. Different lifetimes
// and different authorities — see migration 003.
func (s *Store) Orientation(ctx context.Context, projectID string) (string, error) {
	var notes string
	err := s.DB.QueryRowContext(ctx, `SELECT orientation FROM project WHERE id = $1`, projectID).Scan(&notes)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read orientation: %w", err)
	}
	return notes, nil
}

// SetOrientation replaces the orientation notes.
//
// Replace rather than append, like Memories: the agent is shown the current text and
// returns the version it wants, so a fact that turned out to be wrong can be
// corrected instead of accumulating next to its correction.
func (s *Store) SetOrientation(ctx context.Context, projectID, notes string) error {
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE project SET orientation = $2, updated_at = now() WHERE id = $1`,
		projectID, strings.TrimSpace(notes)); err != nil {
		return fmt.Errorf("write orientation: %w", err)
	}
	return nil
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
