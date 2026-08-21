package store

import (
	"context"
	"fmt"
)

// ClearedCounts reports what a clear removed.
type ClearedCounts struct {
	Messages  int
	Runs      int
	ToolCalls int
	Events    int
}

// Empty reports whether there was nothing worth clearing.
//
// Events are counted but deliberately not consulted. The handler records a
// conversation_cleared event after every successful clear, into the same table
// this empties, so the next clear always finds at least that one row. Including
// events made "there was nothing to clear" unreachable after the first clear and
// reported "deleted 0 messages and 0 runs" instead.
func (c ClearedCounts) Empty() bool {
	return c.Messages == 0 && c.Runs == 0 && c.ToolCalls == 0
}

// ClearConversation permanently deletes the conversation and everything derived
// from it, leaving the sandbox and the project untouched.
//
// This is the whole of context management. There is no automatic summarisation:
// the operator clears when the work has reached a stable base, which is a
// judgement call a token threshold cannot make. The intended cycle is assign a
// task, approve the plan, let it ship, check the result, clear, assign the next
// one — with the sandbox still warm and the repo still checked out.
//
// It is a hard delete, not an archive. The previous version flipped in_context to
// false and stamped archived_at, which kept the rows readable on a History screen
// but left runs, events, tool call logs and the task board behind: "cleared"
// emptied the model's context without emptying the database, and the leftovers
// were the state a later run tripped over. For a single-operator tool, cluttered
// tables with no reader are worse than lost history.
//
// Deliberately preserved:
//
//   - project.memories, the remember tool's durable notes. Conventions and
//     decisions are meant to outlive the conversation that produced them; that is
//     the entire reason they are a separate column and not messages.
//   - background_process rows. They track real processes still running inside the
//     sandbox. Deleting them would orphan a live dev server: still listening, with
//     no record for stop_bg or read_logs to find it by.
//   - every sandbox column on project — sandbox_id, sandbox_status, preview_port,
//     the branches, the checkout. Clearing is not recreating.
//
// project.scratchpad is reset, because the task board describes the conversation
// being deleted and a stale plan would be presented to the next run as its own.
//
// An active run is rejected. Deleting a live runner's messages between an
// assistant tool request and its tool result would leave the next model call
// holding a request with no answer.
//
// A project with nothing to clear is not an error: the counts come back zero and
// the caller says so.
func (s *Store) ClearConversation(ctx context.Context, projectID string) (ClearedCounts, error) {
	var counts ClearedCounts

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return counts, err
	}
	defer tx.Rollback()

	if err := lockProjectTx(ctx, tx, projectID); err != nil {
		return counts, err
	}

	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run
		WHERE project_id = $1 AND state IN ($2, $3, $4))`,
		projectID, RunRunning, RunAwaitingHuman, RunPaused).Scan(&active); err != nil {
		return counts, fmt.Errorf("check active run while clearing conversation: %w", err)
	}
	if active {
		return counts, ErrActiveRun
	}

	// Ordered by foreign key, innermost first. Nothing in the schema cascades, so
	// each of these is required and the order is not cosmetic.
	//
	// tool_call_log carries no project_id — it is scoped through run, and through
	// message as well because run_id is nullable and a row logged outside a run
	// would otherwise survive as an orphan pointing at a deleted message.
	steps := []struct {
		into *int
		sql  string
	}{
		{&counts.ToolCalls, `DELETE FROM tool_call_log
			WHERE run_id IN (SELECT id FROM run WHERE project_id = $1)
			   OR message_id IN (SELECT id FROM message WHERE project_id = $1)`},
		{&counts.Events, `DELETE FROM event WHERE project_id = $1`},
		{&counts.Messages, `DELETE FROM message WHERE project_id = $1`},
		{&counts.Runs, `DELETE FROM run WHERE project_id = $1`},
	}
	for _, step := range steps {
		res, err := tx.ExecContext(ctx, step.sql, projectID)
		if err != nil {
			return counts, fmt.Errorf("clear conversation: %w", err)
		}
		affected, _ := res.RowsAffected()
		*step.into = int(affected)
	}

	// The task board belongs to the conversation just deleted. memories is left
	// alone on purpose — see the doc comment.
	if _, err := tx.ExecContext(ctx,
		`UPDATE project SET scratchpad = '{}', updated_at = now() WHERE id = $1`,
		projectID); err != nil {
		return counts, fmt.Errorf("clear scratchpad while clearing conversation: %w", err)
	}

	// Always committed, even when the counts are all zero. Returning early here
	// used to roll back the scratchpad reset above, which meant a stale task board
	// with no surviving messages survived a clear — precisely the state the reset
	// exists to prevent. The caller decides what to say about an empty result; it is
	// not an error.
	if err := tx.Commit(); err != nil {
		return counts, fmt.Errorf("commit clear conversation: %w", err)
	}
	return counts, nil
}
