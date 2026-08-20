package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Event types carried over SSE.
//
// Only structural events are persisted. Text deltas are streamed live and never
// written here — see AppendEvent for why.
const (
	EventMessageCreated      = "message.created"
	EventMessageComplete     = "message.complete"
	EventToolStarted         = "tool.started"
	EventToolComplete        = "tool.complete"
	EventAgentState          = "agent.state"
	EventRunState            = "run.state"
	EventPlanUpdated         = "plan.updated"
	EventConversationCleared = "conversation.cleared"
	EventSandboxStatus       = "sandbox.status"
)

// Event is one row of the SSE transport.
type Event struct {
	ID        int64
	ProjectID string
	RunID     sql.NullString
	Type      string
	Payload   json.RawMessage
	CreatedAt time.Time
}

// AppendEvent records a structural event and returns it with its assigned id.
//
// The id is what makes reconnection lossless: it is sent as the SSE event id, the
// browser returns it as Last-Event-ID, and EventsSince resumes from there.
//
// Text deltas deliberately do not come through here. A delta is not information —
// it is an animation of information that is about to be persisted in full as a
// message row. Writing thousands of rows to replay an animation costs storage, adds
// a database write to the hot streaming path, and creates a second version of the
// text that can disagree with the canonical one. Deltas are broadcast live with no
// id, so the browser's Last-Event-ID stays pinned to the last structural event and
// a reconnect resumes from a boundary that still makes sense. The transcript
// rebuilds from message rows either way, which is the rule the whole design rests
// on: the stream is a convenience, never a dependency.
func (s *Store) AppendEvent(ctx context.Context, projectID, runID, eventType string, payload any) (*Event, error) {
	if projectID == "" {
		return nil, fmt.Errorf("append event: project id is required")
	}

	encoded := []byte("{}")
	if payload != nil {
		var err error
		if encoded, err = json.Marshal(payload); err != nil {
			return nil, fmt.Errorf("encode event %s: %w", eventType, err)
		}
	}

	var e Event
	var raw []byte
	err := s.DB.QueryRowContext(ctx, `
		INSERT INTO event (project_id, run_id, type, payload)
		VALUES ($1, $2, $3, $4)
		RETURNING id, project_id, run_id, type, payload, created_at`,
		projectID, nullable(runID), eventType, encoded).
		Scan(&e.ID, &e.ProjectID, &e.RunID, &e.Type, &raw, &e.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("append event: %w", err)
	}
	e.Payload = json.RawMessage(raw)
	return &e, nil
}

// EventsSince returns events newer than an id, oldest first.
func (s *Store) EventsSince(ctx context.Context, projectID string, afterID int64, limit int) ([]*Event, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, project_id, run_id, type, payload, created_at
		  FROM event
		 WHERE project_id = $1 AND id > $2
		 ORDER BY id LIMIT $3`, projectID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()

	var out []*Event
	for rows.Next() {
		var e Event
		var raw []byte
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.RunID, &e.Type, &raw, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.Payload = json.RawMessage(raw)
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return out, nil
}

// EventRetentionDays is how long SSE transport rows are kept.
//
// Events are the one prunable table. They are UI transport, not history: the
// same information exists in message and tool_call_log. Text deltas alone
// generate thousands of rows per goal.
const EventRetentionDays = 7

// PruneEvents deletes event rows older than the retention window.
func (s *Store) PruneEvents(ctx context.Context, log *slog.Logger) error {
	res, err := s.DB.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM event WHERE created_at < now() - interval '%d days'`, EventRetentionDays))
	if err != nil {
		return fmt.Errorf("prune events: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Info("pruned old events", "rows", n, "retention_days", EventRetentionDays)
	}
	return nil
}
