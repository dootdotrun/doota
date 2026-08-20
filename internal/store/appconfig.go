package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// FieldKind determines how a config value is stored, validated, and rendered.
type FieldKind string

const (
	KindText     FieldKind = "text"
	KindTextarea FieldKind = "textarea"
	KindInt      FieldKind = "int"
	KindBool     FieldKind = "bool"
)

// Field describes one runtime-editable setting.
type Field struct {
	Key     string
	Group   string
	Label   string
	Help    string
	Kind    FieldKind
	Default any
}

// ConfigGroups is the display order of setting groups on the Settings screen.
var ConfigGroups = []string{"Model", "Agent", "Sandbox", "Git"}

// ConfigFields is every setting that lives in the database.
//
// Deliberately absent: any machine-spec key. Sprites scale automatically and
// bill on usage, so there is no CPU, memory, or disk size to choose. A config
// key that silently does nothing is worse than its absence.
//
// Also deliberately absent: token prices, context limits, and a spend ceiling.
// This is a single-operator tool. Cost belongs on the provider's billing page,
// and context is managed by clearing the conversation at a stable base rather
// than by a threshold that triggers an automatic summary.
var ConfigFields = []Field{
	{
		Key: "model.name", Group: "Model", Label: "Model", Kind: KindText,
		Default: "muse-spark-1.2",
		Help:    "Model id sent to the API.",
	},
	{
		Key: "model.base_url", Group: "Model", Label: "Base URL", Kind: KindText,
		Default: "https://api.meta.ai/v1",
		Help:    "OpenAI-compatible endpoint.",
	},
	{
		Key: "model.max_output_tokens", Group: "Model", Label: "Max output tokens", Kind: KindInt,
		Default: 16384,
		Help: "Muse Spark is a reasoning model and spends this budget on reasoning first. " +
			"Set it too low and calls return no content at all, having thought until the budget ran out.",
	},
	{
		Key: "agent.system_prompt", Group: "Agent", Label: "System prompt", Kind: KindTextarea,
		Default: DefaultSystemPrompt,
		Help:    "The highest-leverage setting here. Editable without a redeploy.",
	},
	{
		Key: "sandbox.setup_script", Group: "Sandbox", Label: "Setup script", Kind: KindTextarea,
		Default: DefaultSetupScript,
		Help:    "Runs once when a project is created. The filesystem persists, so this is not re-run on wake.",
	},
	{
		Key: "git.author_name", Group: "Git", Label: "Commit author name", Kind: KindText,
		Default: "doot",
	},
	{
		Key: "git.author_email", Group: "Git", Label: "Commit author email", Kind: KindText,
		Default: "doot@localhost",
	},
	// git.auto_pr is deliberately gone. It was declared, rendered on Settings, and
	// never read by anything: done always attempts a pull request and never blocks
	// on the failure. A key that silently does nothing is worse than its absence.
}

// FieldByKey returns the field definition for a key.
func FieldByKey(key string) (Field, bool) {
	for _, f := range ConfigFields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// AppConfig is a loaded snapshot of app_config.
type AppConfig map[string]json.RawMessage

// LoadConfig reads all configuration.
func (s *Store) LoadConfig(ctx context.Context) (AppConfig, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT key, value FROM app_config`)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	defer rows.Close()

	out := AppConfig{}
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan config: %w", err)
		}
		out[k] = json.RawMessage(v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate config: %w", err)
	}
	return out, nil
}

// String returns a string setting, falling back to the field default.
func (c AppConfig) String(key string) string {
	if raw, ok := c[key]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	if f, ok := FieldByKey(key); ok {
		if s, ok := f.Default.(string); ok {
			return s
		}
	}
	return ""
}

// Int returns an integer setting, falling back to the field default.
func (c AppConfig) Int(key string) int {
	if raw, ok := c[key]; ok {
		var n int
		if json.Unmarshal(raw, &n) == nil {
			return n
		}
	}
	if f, ok := FieldByKey(key); ok {
		if n, ok := f.Default.(int); ok {
			return n
		}
	}
	return 0
}

// Bool returns a boolean setting, falling back to the field default.
func (c AppConfig) Bool(key string) bool {
	if raw, ok := c[key]; ok {
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			return b
		}
	}
	if f, ok := FieldByKey(key); ok {
		if b, ok := f.Default.(bool); ok {
			return b
		}
	}
	return false
}

// Display renders a value for a form field.
func (c AppConfig) Display(f Field) string {
	switch f.Kind {
	case KindInt:
		return strconv.Itoa(c.Int(f.Key))
	case KindBool:
		if c.Bool(f.Key) {
			return "true"
		}
		return "false"
	default:
		return c.String(f.Key)
	}
}

// ParseValue validates and converts submitted form input for a field.
func ParseValue(f Field, raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	switch f.Kind {
	case KindInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%s must be a whole number", f.Label)
		}
		if n <= 0 {
			return nil, fmt.Errorf("%s must be greater than zero", f.Label)
		}
		return n, nil
	case KindBool:
		return raw == "true" || raw == "on" || raw == "1", nil
	case KindText, KindTextarea:
		return raw, nil
	default:
		return nil, fmt.Errorf("unknown field kind %q", f.Kind)
	}
}

// SetConfig upserts a single value.
func (s *Store) SetConfig(ctx context.Context, key string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode config %s: %w", key, err)
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO app_config (key, value, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE
		    SET value = excluded.value, updated_at = now()`, key, encoded); err != nil {
		return fmt.Errorf("set config %s: %w", key, err)
	}
	return nil
}

// SetConfigValues upserts many values in one transaction.
func (s *Store) SetConfigValues(ctx context.Context, values map[string]any) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin config update: %w", err)
	}
	defer tx.Rollback()

	for key, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode config %s: %w", key, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO app_config (key, value, updated_at)
			VALUES ($1, $2, now())
			ON CONFLICT (key) DO UPDATE
			    SET value = excluded.value, updated_at = now()`, key, encoded); err != nil {
			return fmt.Errorf("set config %s: %w", key, err)
		}
	}
	return tx.Commit()
}

// EnsureConfigDefaults inserts any missing default, leaving existing values
// alone.
//
// Filled key by key rather than as one blob, so adding a setting in a later
// version needs no migration and never overwrites something already customised.
//
// seeds overrides the compiled-in default for a key, and is how environment
// values like LLM_MODEL reach the database. It only affects rows that do not
// exist yet: once a key is in app_config, Settings is the only thing that
// changes it. That keeps the environment from quietly reverting an edit on every
// deploy, which is the failure mode that makes env-plus-database config painful.
func (s *Store) EnsureConfigDefaults(ctx context.Context, log *slog.Logger, seeds map[string]any) error {
	inserted := 0
	for _, f := range ConfigFields {
		value := f.Default
		if seeded, ok := seeds[f.Key]; ok && seeded != "" {
			value = seeded
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode default for %s: %w", f.Key, err)
		}
		res, err := s.DB.ExecContext(ctx, `
			INSERT INTO app_config (key, value)
			VALUES ($1, $2)
			ON CONFLICT (key) DO NOTHING`, f.Key, encoded)
		if err != nil {
			return fmt.Errorf("insert default for %s: %w", f.Key, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	if inserted > 0 {
		log.Info("inserted config defaults", "count", inserted)
	}
	return nil
}
