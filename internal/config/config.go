// Package config loads process configuration from the environment.
//
// There is exactly one thing this process cannot discover for itself: where its
// database is. Everything else — every credential, every model setting, the
// system prompt, the setup script — lives in the app_config table and is edited
// on the Settings screen. See internal/store.
//
// That split is the whole design. A deployment is one environment variable
// pasted into a dashboard, and nothing else about it requires a redeploy or a
// command line to change. The cost is that a fresh install boots without
// credentials and has to say so; AppConfig.MissingCredentials and the setup
// banner exist for exactly that.
//
// The credential variables this package used to require are still read, but only
// as seeds: on the first boot after they moved into the database they are copied
// into any key that does not exist yet, and can then be deleted from the
// environment. See LegacySeeds and Store.EnsureConfigDefaults.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/store"
)

// Sandbox provider identifiers.
const (
	ProviderSprites = "sprites"
	ProviderLocal   = "local"
)

// Config holds the environment-provided configuration.
type Config struct {
	// DatabaseURL is the only required variable. Read from DATABASE_URL, which is
	// what a Fly Postgres attachment and most managed providers set, falling back
	// to NEON_CONNECTION_STRING for continuity with earlier deployments.
	DatabaseURL string

	Port string

	// SandboxProvider selects the sandbox implementation. "sprites" in any real
	// deployment; "local" exists so the project lifecycle and preview proxy can
	// be verified without a Fly Sprites account. Not a credential, and not in the
	// database, because it describes the host rather than the operator's account —
	// and because a UI toggle that could point an agent at this container's own
	// filesystem is not a toggle worth having.
	SandboxProvider string
	LocalSandboxDir string

	// legacySeeds carries credentials found in the environment so they can be
	// copied into the database once. Unexported: nothing should read a credential
	// from here.
	legacySeeds map[string]any
}

// Load reads and validates the environment.
func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:     firstNonEmpty(os.Getenv("DATABASE_URL"), os.Getenv("NEON_CONNECTION_STRING")),
		Port:            strings.TrimSpace(os.Getenv("PORT")),
		SandboxProvider: strings.ToLower(strings.TrimSpace(os.Getenv("DOOT_SANDBOX_PROVIDER"))),
		LocalSandboxDir: strings.TrimSpace(os.Getenv("DOOT_LOCAL_SANDBOX_DIR")),
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required (NEON_CONNECTION_STRING is also accepted): " +
			"it is the only environment variable this app needs, and everything else is configured on the Settings screen")
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

	c.legacySeeds = legacySeeds()
	return c, nil
}

// ConfigSeeds returns values that should fill runtime-editable keys that do not
// exist yet.
//
// Applied with ON CONFLICT DO NOTHING, so this runs once per key per database.
// Editing a setting in Settings afterwards is permanent and a redeploy will not
// stamp on it.
func (c *Config) ConfigSeeds() map[string]any { return c.legacySeeds }

// SessionSecretSeed returns a SESSION_SECRET found in the environment, if any.
//
// Separate from ConfigSeeds because the session key is not a ConfigFields entry
// and so is not filled by EnsureConfigDefaults. See Store.EnsureSessionSecret.
func (c *Config) SessionSecretSeed() string {
	if v, ok := c.legacySeeds[store.KeySessionSecret].(string); ok {
		return v
	}
	return ""
}

// LegacySeedNames reports which retired environment variables were found, so
// startup can say they are no longer needed.
func (c *Config) LegacySeedNames() []string {
	var names []string
	for _, m := range legacyMappings {
		if strings.TrimSpace(os.Getenv(m.env)) != "" {
			names = append(names, m.env)
		}
	}
	return names
}

// legacyMappings maps retired environment variables to the config keys they now
// live in.
var legacyMappings = []struct {
	env, key string
}{
	{"LLM_API_KEY", store.KeyModelAPIKey},
	{"LLM_API_ENDPOINT", store.KeyModelBaseURL},
	{"LLM_MODEL", store.KeyModelName},
	{"SPRITE_TOKEN", store.KeySpriteToken},
	{"GITHUB_TOKEN", store.KeyGitHubToken},
	{"SESSION_SECRET", store.KeySessionSecret},
}

func legacySeeds() map[string]any {
	seeds := map[string]any{}
	for _, m := range legacyMappings {
		if v := strings.TrimSpace(os.Getenv(m.env)); v != "" {
			seeds[m.key] = v
		}
	}
	return seeds
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
