package store

import (
	"context"
	"fmt"
)

// ProjectToolCalls filters the model-executed forensic log without losing the
// tool-call history when its messages were archived.
func (s *Store) ProjectToolCalls(ctx context.Context, projectID, name string, errorsOnly bool, limit int) ([]*ToolCall, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+toolCallColumns+` FROM tool_call_log t
		JOIN run r ON r.id = t.run_id WHERE r.project_id = $1
		AND ($2 = '' OR t.tool_name = $2) AND (NOT $3 OR t.is_error)
		ORDER BY t.created_at DESC, t.id DESC LIMIT $4`, projectID, name, errorsOnly, limit)
	if err != nil {
		return nil, fmt.Errorf("list project tool calls: %w", err)
	}
	defer rows.Close()
	var out []*ToolCall
	for rows.Next() {
		call, err := scanToolCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, call)
	}
	return out, rows.Err()
}

// ReviewAttempted reports whether the reviewer was invoked during this run.
//
// Attempted, not passed. done uses this for a single nudge when the model tries to
// ship without a review, and counting attempts is what stops that nudge becoming a
// loop when the reviewer itself is failing. It is deliberately not the old phase
// contract: no review id to quote, no written justification, no commit to match —
// just "you have not asked for a review yet".
func (s *Store) ReviewAttempted(ctx context.Context, runID string) (bool, error) {
	var exists bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tool_call_log
		WHERE run_id = $1 AND tool_name = 'review')`, runID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check review attempt: %w", err)
	}
	return exists, nil
}
