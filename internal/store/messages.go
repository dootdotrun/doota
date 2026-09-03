package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Message roles, mirroring the message.role check constraint.
//
// internal/model declares the same four for the API's vocabulary. They are the same
// strings by necessity rather than by sharing: one mirrors a database constraint,
// the other a wire format, and neither layer should have to import the other to
// name a role.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message kinds, mirroring the message.kind check constraint. A message with no
// kind is an ordinary turn.
const (
	KindPlan     = "plan"
	KindReview   = "review"
	KindAskHuman = "ask_human"
	KindNotice   = "notice"
)

// Message is one turn of the transcript.
//
// The transcript is append-only. Clearing the conversation flips in_context and
// stamps archived_at; it does not edit or delete.
type Message struct {
	ID        int64
	ProjectID string
	RunID     sql.NullString
	Role      string
	Kind      sql.NullString
	Content   string
	ToolCalls json.RawMessage

	// ReasoningItems is the model's retained chain of thought for this turn, as it
	// came off the wire. Replayed verbatim on later requests and never rendered:
	// see internal/model on why dropping it is expensive.
	ReasoningItems json.RawMessage

	ToolCallID      sql.NullString
	ToolName        sql.NullString
	TokenCount      sql.NullInt64
	PromptTokens    sql.NullInt64
	ReasoningTokens sql.NullInt64
	InContext       bool
	ArchivedAt      sql.NullTime
	Interrupted     bool
	CreatedAt       time.Time

	// ToolDisplay is the tool's richer payload for the UI — a structured diff, a
	// findings list. Never sent to the model, which reads Content instead.
	ToolDisplay json.RawMessage
}

// Display reports whether this message carries a UI payload.
func (m *Message) Display() bool {
	return len(m.ToolDisplay) > 0 && string(m.ToolDisplay) != "null"
}

// MessageKind returns the kind, or "" for an ordinary turn.
func (m *Message) MessageKind() string {
	if m.Kind.Valid {
		return m.Kind.String
	}
	return ""
}

// Tool returns the tool name for a tool-result message.
func (m *Message) Tool() string {
	if m.ToolName.Valid {
		return m.ToolName.String
	}
	return ""
}

// CallID returns the tool call this message answers.
func (m *Message) CallID() string {
	if m.ToolCallID.Valid {
		return m.ToolCallID.String
	}
	return ""
}

// Tokens returns the recorded token count, or 0 when none was recorded.
func (m *Message) Tokens() int {
	if m.TokenCount.Valid {
		return int(m.TokenCount.Int64)
	}
	return 0
}

// HasToolCalls reports whether this assistant turn requested tools.
func (m *Message) HasToolCalls() bool {
	return len(m.ToolCalls) > 0 && string(m.ToolCalls) != "null"
}

// NewMessage is the input to AppendMessage. Optional fields are zero-valued.
type NewMessage struct {
	ProjectID       string
	RunID           string
	Role            string
	Kind            string
	Content         string
	ReasoningItems  json.RawMessage
	ToolCalls       json.RawMessage
	ToolCallID      string
	ToolName        string
	TokenCount      int
	PromptTokens    int
	ReasoningTokens int
	Interrupted     bool
	ToolDisplay     json.RawMessage
}

const messageColumns = `id, project_id, run_id, role, kind, content, reasoning_items,
	tool_calls, tool_call_id, tool_name, token_count, prompt_tokens,
	reasoning_tokens, in_context, archived_at, interrupted, created_at, tool_display`

func scanMessage(row interface{ Scan(...any) error }) (*Message, error) {
	var m Message
	var reasoning, toolCalls, toolDisplay []byte
	err := row.Scan(&m.ID, &m.ProjectID, &m.RunID, &m.Role, &m.Kind,
		&m.Content, &reasoning, &toolCalls, &m.ToolCallID, &m.ToolName,
		&m.TokenCount, &m.PromptTokens, &m.ReasoningTokens,
		&m.InContext, &m.ArchivedAt, &m.Interrupted, &m.CreatedAt,
		&toolDisplay)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan message: %w", err)
	}
	m.ToolCalls = json.RawMessage(toolCalls)
	m.ToolDisplay = json.RawMessage(toolDisplay)
	m.ReasoningItems = json.RawMessage(reasoning)
	return &m, nil
}

// HasReasoning reports whether this turn carries reasoning to replay.
func (m *Message) HasReasoning() bool {
	return len(m.ReasoningItems) > 0 && string(m.ReasoningItems) != "null"
}

// AppendMessage adds a turn to the transcript.
func (s *Store) AppendMessage(ctx context.Context, in NewMessage) (*Message, error) {
	if in.ProjectID == "" {
		return nil, fmt.Errorf("append message: project id is required")
	}
	if in.Role == "" {
		return nil, fmt.Errorf("append message: role is required")
	}

	m, err := scanMessage(s.DB.QueryRowContext(ctx, `
		INSERT INTO message
			(project_id, run_id, role, kind, content, reasoning_items,
			 tool_calls, tool_call_id, tool_name, token_count, prompt_tokens,
			 reasoning_tokens, interrupted, tool_display)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING `+messageColumns,
		in.ProjectID, nullable(in.RunID),
		in.Role, nullable(in.Kind), in.Content, nullableJSON(in.ReasoningItems),
		nullableJSON(in.ToolCalls), nullable(in.ToolCallID), nullable(in.ToolName),
		nullableInt(in.TokenCount), nullableInt(in.PromptTokens),
		nullableInt(in.ReasoningTokens), in.Interrupted, nullableJSON(in.ToolDisplay)))
	if err != nil {
		return nil, fmt.Errorf("append message: %w", err)
	}
	return m, nil
}

// ContextMessages returns the live window in model order.
func (s *Store) ContextMessages(ctx context.Context, projectID string) ([]*Message, error) {
	return s.queryMessages(ctx,
		`SELECT `+messageColumns+` FROM message
		  WHERE project_id = $1 AND in_context ORDER BY id`, projectID)
}

// TranscriptMessages returns the live conversation for the Chat screen, oldest
// first.
//
// Archived messages are deliberately absent: they are still readable on Activity,
// and showing them here would misrepresent what the model can currently see.
func (s *Store) TranscriptMessages(ctx context.Context, projectID string, limit int) ([]*Message, error) {
	if limit <= 0 {
		limit = 500
	}
	// Newest-first with a limit, then reversed, so a long conversation shows its
	// most recent turns rather than its first ones.
	msgs, err := s.queryMessages(ctx,
		`SELECT `+messageColumns+` FROM message
		  WHERE project_id = $1 AND in_context
		  ORDER BY id DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// MessagesAfter returns messages newer than an id, oldest first.
//
// The Chat screen's incremental fetch: the SSE stream says something happened, and
// this returns the canonical rows for it. Rendering stays server-side so the
// browser never becomes a second implementation of the transcript.
func (s *Store) MessagesAfter(ctx context.Context, projectID string, afterID int64) ([]*Message, error) {
	return s.queryMessages(ctx,
		`SELECT `+messageColumns+` FROM message
		  WHERE project_id = $1 AND id > $2 AND in_context
		  ORDER BY id`, projectID, afterID)
}

// ContextTokens returns the input token count of the most recent model call in the
// live window, or 0 if nothing has been called yet.
//
// This is how full the context actually was on the last request, straight from the
// API's own accounting rather than estimated from message lengths. Archived messages
// are excluded for the same reason they are excluded from the transcript: after a
// Clear the window really is empty, and a number that kept counting them would be
// describing a conversation the model can no longer see.
func (s *Store) ContextTokens(ctx context.Context, projectID string) (int, error) {
	var tokens sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT prompt_tokens FROM message
		WHERE project_id = $1 AND in_context AND prompt_tokens IS NOT NULL
		ORDER BY id DESC LIMIT 1`, projectID).Scan(&tokens)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read context tokens: %w", err)
	}
	if !tokens.Valid {
		return 0, nil
	}
	return int(tokens.Int64), nil
}

// LatestMessageID returns the newest message id for a project, or 0 for none.
func (s *Store) LatestMessageID(ctx context.Context, projectID string) (int64, error) {
	var id sql.NullInt64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT max(id) FROM message WHERE project_id = $1`, projectID).Scan(&id); err != nil {
		return 0, fmt.Errorf("latest message id: %w", err)
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// CountMessages returns how many messages a project has, archived included.
func (s *Store) CountMessages(ctx context.Context, projectID string) (int, error) {
	var n int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM message WHERE project_id = $1`, projectID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count messages: %w", err)
	}
	return n, nil
}

func (s *Store) queryMessages(ctx context.Context, query string, args ...any) ([]*Message, error) {
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	return scanMessages(rows)
}

func queryMessagesTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]*Message, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages in transaction: %w", err)
	}
	return scanMessages(rows)
}

func scanMessages(rows *sql.Rows) ([]*Message, error) {
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return out, nil
}

// nullable turns an empty string into a NULL parameter.
func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// nullableInt turns a zero into a NULL parameter.
func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// nullableJSON turns empty JSON into a NULL parameter.
func nullableJSON(v json.RawMessage) any {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	return []byte(v)
}
