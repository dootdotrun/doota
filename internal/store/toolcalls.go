package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ToolCall is one row of the forensic tool log.
type ToolCall struct {
	ID            string
	RunID         sql.NullString
	MessageID     sql.NullInt64
	ToolName      string
	Args          json.RawMessage
	ResultContent sql.NullString
	ResultDisplay json.RawMessage
	IsError       bool
	DurationMS    sql.NullInt64
	CreatedAt     time.Time
}

// Content returns the result text the model saw.
func (t *ToolCall) Content() string {
	if t.ResultContent.Valid {
		return t.ResultContent.String
	}
	return ""
}

// Duration returns how long the call took, or zero if it was not recorded.
func (t *ToolCall) Duration() time.Duration {
	if t.DurationMS.Valid {
		return time.Duration(t.DurationMS.Int64) * time.Millisecond
	}
	return 0
}

// Run returns the owning run id, or "" for a call made outside a run.
func (t *ToolCall) Run() string {
	if t.RunID.Valid {
		return t.RunID.String
	}
	return ""
}

// ToolCallRecord is what a caller supplies to record an execution.
//
// RunID and MessageID are optional because tools may run before a durable
// transcript message is available; the columns remain nullable for those safe
// infrastructure boundaries.
type ToolCallRecord struct {
	RunID     string
	MessageID int64
	ToolName  string
	Args      json.RawMessage
	Content   string
	Display   any
	IsError   bool
	Duration  time.Duration
}

const toolCallColumns = `id, run_id, message_id, tool_name, args, result_content,
	result_display, is_error, duration_ms, created_at`

func scanToolCall(row interface{ Scan(...any) error }) (*ToolCall, error) {
	var t ToolCall
	var args, display []byte
	err := row.Scan(&t.ID, &t.RunID, &t.MessageID, &t.ToolName, &args,
		&t.ResultContent, &display, &t.IsError, &t.DurationMS, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan tool call: %w", err)
	}
	t.Args = json.RawMessage(args)
	t.ResultDisplay = json.RawMessage(display)
	return &t, nil
}

// LogToolCall records one tool execution and returns the new row id.
//
// Every execution is logged, including failures and calls the model got wrong.
// This is deliberately independent of the message history: it survives Clear
// Conversation, so a run stays diagnosable long after the conversation that
// produced it has been archived out of the model's context.
func (s *Store) LogToolCall(ctx context.Context, rec ToolCallRecord) (string, error) {
	var runID any
	if rec.RunID != "" {
		runID = rec.RunID
	}
	var messageID any
	if rec.MessageID != 0 {
		messageID = rec.MessageID
	}

	var args any
	if len(rec.Args) > 0 {
		args = []byte(rec.Args)
	}

	// A Display that will not marshal is logged as absent rather than failing the
	// write. Losing the richer UI payload is a cosmetic problem; losing the record
	// that the call happened at all is not.
	var display any
	if rec.Display != nil {
		if encoded, err := json.Marshal(rec.Display); err == nil {
			display = encoded
		}
	}

	var id string
	err := s.DB.QueryRowContext(ctx, `
		INSERT INTO tool_call_log
			(run_id, message_id, tool_name, args, result_content, result_display,
			 is_error, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		runID, messageID, rec.ToolName, args, rec.Content, display,
		rec.IsError, rec.Duration.Milliseconds()).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("log tool call: %w", err)
	}
	return id, nil
}

// RecentToolCalls returns the newest tool calls, newest first.
func (s *Store) RecentToolCalls(ctx context.Context, limit int) ([]*ToolCall, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+toolCallColumns+` FROM tool_call_log ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list tool calls: %w", err)
	}
	defer rows.Close()

	var out []*ToolCall
	for rows.Next() {
		t, err := scanToolCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tool calls: %w", err)
	}
	return out, nil
}

// CountToolCalls returns how many times a tool has been executed.
func (s *Store) CountToolCalls(ctx context.Context, toolName string) (int, error) {
	var n int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM tool_call_log WHERE tool_name = $1`, toolName).Scan(&n); err != nil {
		return 0, fmt.Errorf("count tool calls: %w", err)
	}
	return n, nil
}
