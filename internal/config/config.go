// Package config loads process configuration from the environment.
//
// Only secrets and bootstrap values live here. Everything else is runtime-
// editable and lives in the app_config table; see internal/store.
//
// Two of these variables — LLM_API_ENDPOINT and LLM_MODEL — name things that are
// *also* runtime-editable settings (model.base_url, model.name). They are not a
// competing source of truth: they seed those keys the first time the database is
// filled, and app_config wins from then on. See Store.EnsureConfigDefaults.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Sandbox provider identifiers.
const (
	ProviderSprites = "sprites"
	ProviderLocal   = "local"
)

// Config holds the environment-provided configuration.
type Config struct {
	DatabaseURL   string
	SessionSecret string
	ModelAPIKey   string
	ModelEndpoint string
	ModelName     string
	SpriteToken   string
	Port          string

	// GitHubToken is a scoped personal access token with repo scope. It is the
	// single credential for every GitHub operation: git clone, fetch, and push
	// use it through a credential file inside the sandbox, and pull requests use
	// it directly against api.github.com from this process.
	//
	// Handing a PAT to the sandbox is a deliberate, accepted tradeoff for a
	// single-operator internal tool. The alternative was the Sprites API Gateway,
	// which proxies GitHub's REST API but not the git wire protocol — so it could
	// open a pull request but could never push the commits the PR was for.
	GitHubToken string

	// SandboxProvider selects the sandbox implementation. "sprites" in any real
	// deployment; "local" exists so the project lifecycle and preview proxy can
	// be verified without a Fly Sprites account.
	SandboxProvider string
	LocalSandboxDir string

	// SessionSecretEphemeral records that no SESSION_SECRET was supplied and one
	// was generated for this process. Sessions then die with the process. The
	// caller is expected to say so loudly at startup.
	SessionSecretEphemeral bool
}

// Secret is an environment-provided credential, surfaced on the Settings screen
// by name and presence only. Values are never rendered.
type Secret struct {
	Name    string
	Purpose string
	Present bool
}

const minSecretLen = 32

// Load reads and validates the environment.
//
// Every required variable must be present. A process that starts without them
// would fail later, mid-run, at a much less obvious moment.
func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:     os.Getenv("NEON_CONNECTION_STRING"),
		SessionSecret:   os.Getenv("SESSION_SECRET"),
		ModelAPIKey:     os.Getenv("LLM_API_KEY"),
		ModelEndpoint:   os.Getenv("LLM_API_ENDPOINT"),
		ModelName:       os.Getenv("LLM_MODEL"),
		SpriteToken:     os.Getenv("SPRITE_TOKEN"),
		GitHubToken:     strings.TrimSpace(os.Getenv("GITHUB_TOKEN")),
		Port:            os.Getenv("PORT"),
		SandboxProvider: strings.ToLower(strings.TrimSpace(os.Getenv("DOOT_SANDBOX_PROVIDER"))),
		LocalSandboxDir: os.Getenv("DOOT_LOCAL_SANDBOX_DIR"),
	}
	if c.Port == "" {
		c.Port = "8080"
	}
	if c.SandboxProvider == "" {
		c.SandboxProvider = ProviderSprites
	}
	if c.SandboxProvider != ProviderSprites && c.SandboxProvider != ProviderLocal {
		return nil, fmt.Errorf("DOOT_SANDBOX_PROVIDER must be %q or %q, got %q",
			ProviderSprites, ProviderLocal, c.SandboxProvider)
	}
	if c.LocalSandboxDir == "" {
		c.LocalSandboxDir = "/tmp/doot-sandboxes"
	}

	// SESSION_SECRET is deliberately not in this list. It is not a credential to
	// anything external — it signs our own cookies — so a missing one can be
	// generated rather than being a reason to refuse to boot. Everything else
	// names a service we cannot invent access to.
	//
	// SPRITE_TOKEN stays required even under the local provider. Needing it only
	// in one mode would mean a deployment could boot and then fail at the first
	// project creation, which is exactly the late failure this function exists to
	// prevent.
	// GITHUB_TOKEN is required. Pushing a branch and opening a pull request is the
	// whole point of the tool, and discovering the credential is absent at the end
	// of a completed goal — after the model has done all the work — is the worst
	// possible moment to find out.
	var missing []string
	for name, v := range map[string]string{
		"NEON_CONNECTION_STRING": c.DatabaseURL,
		"LLM_API_KEY":            c.ModelAPIKey,
		"LLM_API_ENDPOINT":       c.ModelEndpoint,
		"LLM_MODEL":              c.ModelName,
		"SPRITE_TOKEN":           c.SpriteToken,
		"GITHUB_TOKEN":           c.GitHubToken,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	// A supplied secret still has to be long enough to be worth having. A
	// generated one is always long enough, and forces every session to be
	// re-established after a restart — acceptable for a single-user tool, and
	// much better than refusing to start.
	switch {
	case strings.TrimSpace(c.SessionSecret) == "":
		generated, err := randomSecret()
		if err != nil {
			return nil, fmt.Errorf("generate session secret: %w", err)
		}
		c.SessionSecret = generated
		c.SessionSecretEphemeral = true
	case len(c.SessionSecret) < minSecretLen:
		return nil, fmt.Errorf("SESSION_SECRET must be at least %d characters, got %d", minSecretLen, len(c.SessionSecret))
	}

	return c, nil
}

// randomSecret returns a 32-byte secret, hex-encoded to 64 characters.
func randomSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Secrets reports the environment-provided credentials and whether each is set.
//
// Load already refuses to start when any is missing, so in a running process
// these are all present. It is still worth showing: it confirms which secrets
// this process actually loaded, which is the question you have when something
// authenticates unexpectedly.
func (c *Config) Secrets() []Secret {
	sessionPurpose := "Session cookie signing key"
	if c.SessionSecretEphemeral {
		sessionPurpose = "Session cookie signing key (generated — sessions end at restart)"
	}
	return []Secret{
		{"NEON_CONNECTION_STRING", "Postgres (Neon) connection string", c.DatabaseURL != ""},
		{"SESSION_SECRET", sessionPurpose, !c.SessionSecretEphemeral},
		{"LLM_API_KEY", "Model API key", c.ModelAPIKey != ""},
		{"LLM_API_ENDPOINT", "OpenAI-compatible model endpoint", c.ModelEndpoint != ""},
		{"LLM_MODEL", "Default model id", c.ModelName != ""},
		{"SPRITE_TOKEN", "Fly Sprites API token", c.SpriteToken != ""},
		{"GITHUB_TOKEN", "GitHub PAT for clone, push, and pull requests", c.GitHubToken != ""},
	}
}

// ConfigSeeds maps runtime-editable config keys to the environment values that
// should seed them on a fresh database.
//
// Only keys that genuinely have an environment counterpart appear here. The
// values are applied with ON CONFLICT DO NOTHING, so editing them in Settings
// later is permanent and a redeploy will not stamp on it.
func (c *Config) ConfigSeeds() map[string]any {
	return map[string]any{
		"model.name":     c.ModelName,
		"model.base_url": c.ModelEndpoint,
	}
}
