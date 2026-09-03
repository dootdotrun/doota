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
	attempts, _, err := s.ReviewOutcomes(ctx, runID)
	return attempts > 0, err
}

// ReviewOutcomes reports how many reviews this run asked for and whether any of them
// actually reached a verdict.
//
// The distinction is the point. Counting attempts alone was the whole of the pre-ship
// check, which was safe only for as long as attempting a review implied getting one.
// It did not: the reviewer's turn budget was small enough that "ran out of turns
// without reaching a conclusion" was its ordinary outcome, so work shipped with a
// review in the log and nobody having formed an opinion.
//
// Concluded means a review tool call that returned without error — the reviewer said
// CLEAN or listed findings. An errored review is one that could not finish: no diff,
// a broken reviewer, a stream that died.
func (s *Store) ReviewOutcomes(ctx context.Context, runID string) (attempts int, concluded bool, err error) {
	row := s.DB.QueryRowContext(ctx, `SELECT count(*), coalesce(bool_or(NOT is_error), false)
		FROM tool_call_log WHERE run_id = $1 AND tool_name = 'review'`, runID)
	if err := row.Scan(&attempts, &concluded); err != nil {
		return 0, false, fmt.Errorf("check review outcomes: %w", err)
	}
	return attempts, concluded, nil
}
