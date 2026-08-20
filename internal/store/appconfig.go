package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FieldKind determines how a config value is stored, validated, and rendered.
type FieldKind string

const (
	KindText     FieldKind = "text"
	KindTextarea FieldKind = "textarea"
	KindInt      FieldKind = "int"
	KindBool     FieldKind = "bool"

	// KindSecret is a credential. It is stored like text and validated like text,
	// but it is never rendered back to the browser: the Settings form shows only
	// whether it is set, and an empty submission means "leave it alone" rather
	// than "clear it". Without that second rule, saving the form to change the
	// model name would silently erase every token.
	KindSecret FieldKind = "secret"
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

// Secret reports whether the field holds a credential.
func (f Field) Secret() bool { return f.Kind == KindSecret }

// ConfigGroups is the display order of setting groups on the Settings screen.
var ConfigGroups = []string{"Credentials", "Model", "Agent", "Sandbox", "Git"}

// Credential keys. Named constants because these are read from several packages
// and a typo in a string literal would read as "not configured" rather than
// failing loudly.
const (
	KeyModelAPIKey  = "model.api_key"
	KeyModelBaseURL = "model.base_url"
	KeyModelName    = "model.name"
	KeySpriteToken  = "sprites.token"
	KeyGitHubToken  = "github.token"

	// KeySessionSecret is deliberately absent from ConfigFields. It is generated
	// on first boot and never shown or typed, so putting it on the Settings form
	// would only create a way to break every session by pasting something short.
	// See EnsureSessionSecret.
	KeySessionSecret = "session.secret"
)

// minSessionSecretLen is the shortest signing key worth having. A generated one
// is always twice this.
const minSessionSecretLen = 32

// ConfigFields is every setting that lives in the database.
//
// This is the whole configuration surface of the deployment. The process reads
// exactly one environment variable — the database URL — and everything else,
// credentials included, is a row here and a form field on the Settings screen.
// The tradeoff is deliberate: an operator who is not the author should be able
// to stand this up with one value pasted into a dashboard, and change anything
// else without a redeploy.
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
		Key: KeyModelAPIKey, Group: "Credentials", Label: "Model API key", Kind: KindSecret,
		Default: "",
		Help:    "Sent as the bearer token to the endpoint below.",
	},
	{
		Key: KeySpriteToken, Group: "Credentials", Label: "Fly Sprites token", Kind: KindSecret,
		Default: "",
		Help:    "Creates and runs the sandbox each project is built in.",
	},
	{
		Key: KeyGitHubToken, Group: "Credentials", Label: "GitHub token", Kind: KindSecret,
		Default: "",
		Help:    "Classic or fine-grained PAT with repo scope. Used for clone, push, and pull requests.",
	},
	{
		Key: KeyModelName, Group: "Model", Label: "Model", Kind: KindText,
		Default: "muse-spark-1.2",
		Help:    "Model id sent to the API.",
	},
	{
		Key: KeyModelBaseURL, Group: "Model", Label: "Base URL", Kind: KindText,
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

// requiredCredentials are the credentials without which some part of the tool
// cannot work at all. Used to drive the setup banner, so a fresh deployment says
// what is missing instead of failing at the first model call.
var requiredCredentials = []struct {
	Key, Label string
}{
	{KeyModelAPIKey, "Model API key"},
	{KeySpriteToken, "Fly Sprites token"},
	{KeyGitHubToken, "GitHub token"},
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
//
// Treat a snapshot returned by LoadConfig as read-only. It is shared by every
// caller holding it until the next write, so mutating it would silently change
// configuration for an in-flight agent step somewhere else.
type AppConfig map[string]json.RawMessage

// configCacheTTL bounds how long a snapshot is trusted without rechecking the
// database.
//
// Writes through this process invalidate immediately, so this is not the
// mechanism that makes Settings edits take effect — that is instant. It exists
// only for the window during a deploy when two machines are briefly alive and
// the one being replaced could otherwise serve a stale snapshot indefinitely.
const configCacheTTL = 30 * time.Second

type configCache struct {
	mu       sync.RWMutex
	snapshot AppConfig
	loadedAt time.Time

	// generation increments on every invalidation.
	//
	// It exists because a load and a write can overlap: a request that reads the
	// rows, then waits on the network while a Settings save commits and
	// invalidates, would otherwise install its pre-commit snapshot *after* the
	// invalidation and serve it for the full TTL. That is the worst possible
	// window — the operator has just corrected a credential and their next action
	// is to retry the thing that failed. A reader captures the generation before
	// it queries and discards its result if the world moved underneath it.
	generation uint64
}

func (c *configCache) get() (AppConfig, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.snapshot == nil || time.Since(c.loadedAt) > configCacheTTL {
		return nil, false
	}
	return c.snapshot, true
}

// begin returns the generation a caller must present to put.
func (c *configCache) begin() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.generation
}

// put caches a snapshot, unless it was already stale when it arrived.
func (c *configCache) put(cfg AppConfig, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != gen {
		return
	}
	c.snapshot = cfg
	c.loadedAt = time.Now()
}

func (c *configCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = nil
	c.generation++
}

// LoadConfig returns the configuration snapshot, from memory when possible.
//
// The database is queried only when nothing is cached, when a write through this
// process invalidated the cache, or when the snapshot has aged past
// configCacheTTL. Every other call is a map read behind an RWMutex.
func (s *Store) LoadConfig(ctx context.Context) (AppConfig, error) {
	if cfg, ok := s.cfgCache.get(); ok {
		return cfg, nil
	}
	// Captured before the query, so a write that lands while it is in flight makes
	// this result unusable rather than authoritative.
	gen := s.cfgCache.begin()
	cfg, err := s.loadConfigUncached(ctx)
	if err != nil {
		return nil, err
	}
	s.cfgCache.put(cfg, gen)
	return cfg, nil
}

// InvalidateConfig drops the cached snapshot. Writes call this for themselves;
// it is exported for the rare caller that changes app_config by another route.
func (s *Store) InvalidateConfig() { s.cfgCache.invalidate() }

func (s *Store) loadConfigUncached(ctx context.Context) (AppConfig, error) {
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

// Text returns a trimmed string setting.
//
// ParseValue already trims on the way in, so this is belt and braces for values
// that arrived by another route — a seed, a compiled-in default, or a row written
// before the trim existed. A model id with a trailing space passes every
// non-empty check and then fails at the API with an unhelpful error.
func (c AppConfig) Text(key string) string {
	return strings.TrimSpace(c.String(key))
}

// Secret returns a credential, trimmed. Distinct from Text only in intent: it
// marks the call sites where a credential is read.
func (c AppConfig) Secret(key string) string {
	return c.Text(key)
}

// IsSet reports whether a value is present and non-empty. Used to render "set"
// against a secret without rendering the secret.
func (c AppConfig) IsSet(key string) bool {
	return strings.TrimSpace(c.String(key)) != ""
}

// MissingCredentials names the required credentials that are not configured.
//
// Empty means the tool is fully operational. Anything else is rendered as a
// single line pointing at Settings, which is a better answer than the alternative
// — booting happily and failing at the first model call or the first clone.
func (c AppConfig) MissingCredentials() []string {
	var missing []string
	for _, r := range requiredCredentials {
		if !c.IsSet(r.Key) {
			missing = append(missing, r.Label)
		}
	}
	return missing
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
//
// A secret renders as the empty string, always. The form field for a secret is
// therefore blank whether or not one is stored, and "is it set?" is answered
// beside it by IsSet.
func (c AppConfig) Display(f Field) string {
	switch f.Kind {
	case KindSecret:
		return ""
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
	case KindText, KindTextarea, KindSecret:
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
	s.cfgCache.invalidate()
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
	if err := tx.Commit(); err != nil {
		return err
	}
	// After the commit, never before: an invalidation on a rolled-back write would
	// throw away a good snapshot for nothing.
	s.cfgCache.invalidate()
	return nil
}

// EnsureConfigDefaults inserts any missing default, leaving existing values
// alone.
//
// Filled key by key rather than as one blob, so adding a setting in a later
// version needs no migration and never overwrites something already customised.
//
// seeds overrides the compiled-in default for a key. It exists so a deployment
// that used to carry credentials in the environment copies them into the
// database on the first boot after they moved, and the environment variables can
// then be deleted. It only affects rows that do not exist yet: once a key is in
// app_config, Settings is the only thing that changes it. That keeps the
// environment from quietly reverting an edit on every deploy, which is the
// failure mode that makes env-plus-database config painful.
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
	s.cfgCache.invalidate()
	return nil
}

// EnsureSessionSecret returns the persisted cookie-signing key, generating one
// on first boot.
//
// This used to be SESSION_SECRET in the environment, where leaving it unset —
// the default, for anyone who did not read the startup warning — regenerated it
// on every process start and logged the operator out on every deploy. Persisting
// it removes both the variable and the failure. It is not in ConfigFields
// because there is no reason to ever look at it or type it.
// seed, when non-empty, adopts a secret found elsewhere — in practice the
// retired SESSION_SECRET environment variable — so the sessions issued by the
// previous deployment survive the move into the database.
func (s *Store) EnsureSessionSecret(ctx context.Context, log *slog.Logger, seed string) (string, error) {
	cfg, err := s.loadConfigUncached(ctx)
	if err != nil {
		return "", err
	}
	if secret := cfg.Secret(KeySessionSecret); len(secret) >= minSessionSecretLen {
		return secret, nil
	}

	if seed = strings.TrimSpace(seed); len(seed) >= minSessionSecretLen {
		if err := s.SetConfig(ctx, KeySessionSecret, seed); err != nil {
			return "", err
		}
		log.Info("adopted the session signing secret from the environment and stored it in the database")
		return seed, nil
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session secret: %w", err)
	}
	secret := hex.EncodeToString(buf)
	if err := s.SetConfig(ctx, KeySessionSecret, secret); err != nil {
		return "", err
	}
	log.Info("generated a session signing secret and stored it in the database")
	return secret, nil
}
