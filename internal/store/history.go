package store

import (
	"context"
	"fmt"
)

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
