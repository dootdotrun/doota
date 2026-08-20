// Command doot runs the doot-ai server.
//
// Startup is ordered so that failures happen before traffic is accepted: a
// process serving requests against an unmigrated schema is a worse outcome than
// one that refuses to boot.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dootdotrun/doot-ai/internal/agent"
	"github.com/dootdotrun/doot-ai/internal/config"
	"github.com/dootdotrun/doot-ai/internal/events"
	"github.com/dootdotrun/doot-ai/internal/model"
	"github.com/dootdotrun/doot-ai/internal/project"
	"github.com/dootdotrun/doot-ai/internal/sandbox"
	"github.com/dootdotrun/doot-ai/internal/store"
	"github.com/dootdotrun/doot-ai/internal/tools"
	"github.com/dootdotrun/doot-ai/internal/web"
)

// shutdownGrace bounds how long in-flight requests get to finish on SIGTERM.
// Every Fly deploy sends one.
const shutdownGrace = 20 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(log); err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// Cancelled on SIGINT/SIGTERM so shutdown is graceful rather than abrupt.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 1. Environment. One required variable: the database URL. Everything else,
	//    credentials included, is a row in app_config edited on the Settings screen.
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log.Info("configuration loaded", "port", cfg.Port, "sandbox_provider", cfg.SandboxProvider)
	if retired := cfg.LegacySeedNames(); len(retired) > 0 {
		log.Warn("found retired environment variables; they seeded the database on a key that did not exist "+
			"yet and can now be removed from the deployment", "variables", retired)
	}

	// 2. Database. Fatal: without the store there is no loop and no degraded mode.
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	log.Info("database connected")

	// 3. Migrations. Fatal.
	if err := st.Migrate(ctx, log); err != nil {
		return err
	}

	// 4. Default user, if this instance has none yet.
	if err := st.BootstrapUser(ctx, log); err != nil {
		return err
	}

	// 5. Config defaults, filled key by key so new settings need no migration
	//    and never overwrite something already customised. Environment values
	//    seed their matching keys on a fresh database only.
	if err := st.EnsureConfigDefaults(ctx, log, cfg.ConfigSeeds()); err != nil {
		return err
	}

	// 6. Drop stale SSE transport rows. Non-fatal: this is housekeeping.
	if err := st.PruneEvents(ctx, log); err != nil {
		log.Error("prune events", "error", err)
	}

	// 7. Provisioning is not resumable until the durable runner lands in Phase 5,
	//    so anything caught mid-setup by a restart is marked failed rather than
	//    left claiming a readiness it never reached.
	if n, err := st.FailInterruptedProvisioning(ctx); err != nil {
		log.Error("reconcile interrupted provisioning", "error", err)
	} else if n > 0 {
		log.Warn("marked interrupted provisioning as failed", "projects", n)
	}

	// 8. Sandbox provider. The Sprites token is read from configuration on each
	//    operation, through the cached snapshot, so it is a closure rather than a
	//    value captured here.
	spriteToken := func(ctx context.Context) (string, error) {
		appCfg, err := st.LoadConfig(ctx)
		if err != nil {
			return "", err
		}
		return appCfg.Secret(store.KeySpriteToken), nil
	}

	provider, err := buildSandboxProvider(cfg, spriteToken, log)
	if err != nil {
		return err
	}
	defer provider.Close()

	projects := project.New(st, provider, log)

	// 9. The model client, the tool registry, the event hub, and the loop that
	//    ties them together. None of these can fail: the model endpoint is not
	//    contacted until the first message, because refusing to boot over a model
	//    outage would take the whole app down for something the user can fix in
	//    Settings.
	hub := events.NewHub(log)
	registry := tools.Primary()
	modelClient := model.New(log)
	reviewer := tools.Reviewer()
	agents := agent.New(st, projects, modelClient, registry, reviewer, hub, log)
	if err := agents.Recover(ctx); err != nil {
		return fmt.Errorf("recover interrupted runs: %w", err)
	}
	log.Info("agent ready", "tools", registry.Len())

	// 9b. Report what still needs configuring. A fresh deployment is expected to
	//     reach this point with nothing set: the operator's next step is the
	//     Settings screen, and the app says so there too.
	if appCfg, err := st.LoadConfig(ctx); err == nil {
		if missing := appCfg.MissingCredentials(); len(missing) > 0 {
			log.Warn("credentials are not configured yet; set them on the Settings screen", "missing", missing)
		}
	}

	// 10. Serve. The cookie signing key is persisted rather than environmental, so
	//     sessions survive a deploy instead of ending with the process.
	sessionSecret, err := st.EnsureSessionSecret(ctx, log, cfg.SessionSecretSeed())
	if err != nil {
		return err
	}

	srv, err := web.New(st, projects, agents, hub, sessionSecret, log)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: it would cut off the SSE stream added in Phase 4.
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
	}

	// Do not abandon a model/tool boundary during a deploy. The runner finishes
	// the current step if possible and releases its lease for the incoming machine.
	agents.Shutdown(shutdownCtx)

	log.Info("stopped")
	return nil
}

// buildSandboxProvider selects the sandbox implementation.
func buildSandboxProvider(cfg *config.Config, spriteToken func(context.Context) (string, error),
	log *slog.Logger) (sandbox.Provider, error) {
	switch cfg.SandboxProvider {
	case config.ProviderLocal:
		// Loud on purpose: a local provider gives an agent the host filesystem
		// rather than an isolated VM, so nobody should discover this by accident.
		log.Warn("using the LOCAL sandbox provider - directories on this host, no isolation",
			"dir", cfg.LocalSandboxDir)
		return sandbox.NewLocalProvider(cfg.LocalSandboxDir)

	case config.ProviderSprites:
		log.Info("using the Fly Sprites sandbox provider")
		return sandbox.NewSpritesProvider(spriteToken), nil

	default:
		return nil, fmt.Errorf("unknown sandbox provider %q", cfg.SandboxProvider)
	}
}
