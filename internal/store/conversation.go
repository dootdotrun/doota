package store

import (
	"context"
	"fmt"
)

// ClearConversation archives every live message so the next user turn starts
// fresh.
//
// This is the whole of context management. There is no automatic summarisation:
// the operator clears the conversation when the work has reached a stable base,
// which is a judgement call a token threshold cannot make. Nothing is deleted —
// in_context goes false and archived_at is stamped, so the transcript stays
// intact in Postgres.
//
// An active run is rejected. Archiving a live runner's context between an
// assistant tool request and its tool result would leave the next model call
// holding a request with no answer.
func (s *Store) ClearConversation(ctx context.Context, projectID string) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := lockProjectTx(ctx, tx, projectID); err != nil {
		return 0, err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run
		WHERE project_id = $1 AND state IN ($2, $3, $4))`,
		projectID, RunRunning, RunAwaitingHuman, RunPaused).Scan(&active); err != nil {
		return 0, fmt.Errorf("check active run while clearing conversation: %w", err)
	}
	if active {
		return 0, ErrActiveRun
	}
	res, err := tx.ExecContext(ctx, `UPDATE message SET in_context = false, archived_at = now()
		WHERE project_id = $1 AND in_context`, projectID)
	if err != nil {
		return 0, fmt.Errorf("archive conversation: %w", err)
	}
	archived, _ := res.RowsAffected()
	if archived == 0 {
		return 0, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit clear conversation: %w", err)
	}
	return int(archived), nil
}

// ArchivedMessages returns rows the operator has cleared, oldest first. The
// transcript screen shows live messages only; this is the way back to the rest.
func (s *Store) ArchivedMessages(ctx context.Context, projectID string, limit int) ([]*Message, error) {
	if limit <= 0 {
		limit = 200
	}
	return s.queryMessages(ctx, `SELECT `+messageColumns+` FROM message
		WHERE project_id = $1 AND NOT in_context ORDER BY id DESC LIMIT $2`, projectID, limit)
}
