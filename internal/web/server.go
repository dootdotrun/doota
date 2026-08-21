// Package web serves the screens and the actions behind them.
//
// Server-rendered HTML with htmx for partial updates. No JSON API: the only
// consumer is the app's own HTML.
package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/dootdotrun/doot-ai/internal/agent"
	"github.com/dootdotrun/doot-ai/internal/events"
	"github.com/dootdotrun/doot-ai/internal/project"
	"github.com/dootdotrun/doot-ai/internal/store"
	"github.com/dootdotrun/doot-ai/internal/web/session"
)

//go:embed templates/*.html templates/fragments/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// sessionTTL is deliberately long. Re-authenticating a personal tool on a phone
// every few hours is friction with no threat model behind it.
const sessionTTL = 30 * 24 * time.Hour

// appPrefix is where the doot UI lives. Everything outside it belongs to the
// preview proxy.
//
// This inverts the original arrangement, where the UI owned the root and the
// preview was mounted at /preview. That way round, two problems had no fix:
//
//   - The previewed app's own URLs were wrong. A dev server serving
//     `<script src="/assets/app.js">`, calling `fetch("/api/thing")`, pushing
//     /dashboard through the history API, or setting a cookie with Path=/ emits
//     root-absolute URLs, and nothing rewrites a response body on the way back.
//     The browser asked doot for those paths, and doot answered with its own 404
//     or a redirect to its login screen. Serving the preview from the root makes
//     every one of them correct by construction, with no body rewriting, no
//     X-Forwarded-Prefix for the framework to honour, and no Referer guessing.
//   - The preview could not escape the installed PWA. Navigations inside the
//     manifest's scope open in the app's own chrome-less window, which has no
//     back affordance, and /preview was inside a scope of "/". With the UI under
//     /app the manifest scopes to /app/, so the preview is out of scope and opens
//     in a real browser tab.
//
// The cost is that a previewed path beginning /app is unreachable. For one
// operator previewing their own dev server that is a fair trade, and it is the
// only path that is shadowed.
const appPrefix = "/app"

// Server holds the HTTP layer's dependencies.
type Server struct {
	store     *store.Store
	projects  *project.Service
	log       *slog.Logger
	sess      *session.Manager
	pages     map[string]*template.Template
	fragments map[string]*template.Template
	limiter   *loginLimiter

	agent  *agent.Service
	events *events.Hub
}

// New builds the server, parsing templates up front so a broken template fails
// at boot rather than on the request that happens to use it.
func New(st *store.Store, projects *project.Service,
	agents *agent.Service, hub *events.Hub, sessionSecret string, log *slog.Logger) (*Server, error) {
	pages, fragments, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{
		store:     st,
		projects:  projects,
		log:       log,
		sess:      session.NewManager(sessionSecret, sessionTTL),
		pages:     pages,
		fragments: fragments,
		limiter:   newLoginLimiter(),
		agent:     agents,
		events:    hub,
	}, nil
}

// standalone pages render their own complete document; the rest are wrapped in
// the app shell.
var standalonePages = map[string]bool{"login": true}

// templateFuncs are available to every template.
var templateFuncs = template.FuncMap{
	// app prefixes a UI path with the mount point, so appPrefix is the only place
	// "/app" is written. The templates used to hand-copy it about thirty times,
	// which made changing the constant compile cleanly and produce a broken UI.
	"app": func(path string) string { return appPrefix + path },

	// shortID trims an identifier for display without hiding it entirely.
	"shortID": func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	},
}

func parseTemplates() (pages, fragments map[string]*template.Template, err error) {
	pageNames := []string{"login", "chat", "settings"}
	pages = make(map[string]*template.Template, len(pageNames))

	for _, name := range pageNames {
		file := "templates/" + name + ".html"
		t := template.New(name).Funcs(templateFuncs)

		if standalonePages[name] {
			t, err = t.ParseFS(templateFS, file)
		} else {
			// Fragments are parsed alongside pages so a page can render the same
			// partial the polling endpoint returns, with no duplicated markup.
			t, err = t.ParseFS(templateFS, "templates/layout.html", file, "templates/fragments/*.html")
		}
		if err != nil {
			return nil, nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		pages[name] = t
	}

	fragmentNames := []string{"project_status", "messages"}
	fragments = make(map[string]*template.Template, len(fragmentNames))
	for _, name := range fragmentNames {
		t, err := template.New(name).Funcs(templateFuncs).
			ParseFS(templateFS, "templates/fragments/*.html")
		if err != nil {
			return nil, nil, fmt.Errorf("parse fragment %s: %w", name, err)
		}
		fragments[name] = t
	}
	return pages, fragments, nil
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(s.requestLogger)

	// Outside the app prefix and outside the session, so Fly's health check does
	// not depend on the UI's routing or on being logged in. It is also the one path
	// the preview cannot have, which is why it is a single well-known word.
	//
	// Registered for every method, not just GET: with only Get here, a POST to
	// /healthz fell through to the preview catch-all, so the path was reserved for
	// one verb and forwarded for the rest.
	r.Handle("/healthz", http.HandlerFunc(s.handleHealthz))

	// The manifest also stays at the origin root, unauthenticated.
	//
	// It is served from /app/ as well, which is the copy that matters because scope
	// is resolved relative to the manifest's own URL. This one exists for installs
	// made before the move: a browser re-fetches the manifest from the URL recorded
	// at install time, and without this that URL now answers with the preview proxy,
	// so an already-installed app could never learn its scope had changed.
	r.Get("/manifest.webmanifest", s.serveStaticFile("manifest.webmanifest", "application/manifest+json"))

	r.Route(appPrefix, func(r chi.Router) {
		// Unauthenticated.
		r.Get("/login", s.handleLoginForm)
		r.Post("/login", s.handleLogin)
		r.Post("/logout", s.handleLogout)

		sub, err := fs.Sub(staticFS, "static")
		if err != nil {
			panic(fmt.Sprintf("static assets: %v", err))
		}
		fileServer := http.StripPrefix(appPrefix+"/static/", http.FileServer(http.FS(sub)))
		r.Handle("/static/*", cacheStatic(fileServer))

		// Served from inside the prefix, because the manifest's scope is resolved
		// relative to the manifest's own URL and the whole point of the prefix is
		// that the installed app scopes to /app/ rather than the whole origin.
		//
		// There is no service worker: the one that used to live here cached seven
		// static assets that already carry a one-hour Cache-Control and, by its own
		// comment, provided no offline mode — so it bought a versioned cache to
		// invalidate and nothing else. The manifest alone gives the icon.
		r.Get("/manifest.webmanifest", s.serveStaticFile("manifest.webmanifest", "application/manifest+json"))

		// Authenticated: the screens and their actions.
		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)

			r.Get("/", s.handleChat)
			r.Post("/chat", s.handleChatSend)
			r.Post("/chat/pause", s.handleChatPause)
			r.Post("/chat/resume", s.handleChatResume)
			r.Get("/chat/tail", s.handleChatTail)

			// The live stream.
			r.Get("/events", s.handleEvents)

			// The plan is rendered on the Chat screen. This redirect exists so an
			// installed home-screen shortcut or a bookmark to the old tab still
			// lands somewhere useful.
			r.Get("/plan", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, appPrefix+"/#plan", http.StatusMovedPermanently)
			})
			r.Post("/plan/approve", s.handlePlanApprove)
			r.Post("/plan/revise", s.handlePlanRevise)
			r.Get("/plan/diff", s.handlePlanDiff)
			r.Get("/settings", s.handleSettings)

			r.Post("/settings/config", s.handleSaveConfig)
			r.Post("/settings/config/reset", s.handleResetConfigField)
			r.Post("/settings/credentials", s.handleSaveCredentials)

			r.Get("/project/status", s.handleProjectStatus)
			r.Post("/project", s.handleCreateProject)
			r.Post("/project/wake", s.handleProjectWake)
			r.Post("/project/recreate", s.handleProjectRecreate)

			r.Post("/project/preview-port", s.handleProjectPreviewPort)
			r.Post("/project/clear-conversation", s.handleProjectClearConversation)
			r.Post("/project/delete", s.handleProjectDelete)
		})
	})

	// Everything else is the preview.
	//
	// Behind the same session as the UI, which is the entire reason it is proxied
	// rather than exposed as the sandbox's own public URL: there is no secret URL
	// to leak and no provider token in the browser. Registered for every method,
	// because a previewed app has forms and API calls, not just GETs.
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Handle("/", http.HandlerFunc(s.handlePreview))
		r.Handle("/*", http.HandlerFunc(s.handlePreview))
	})

	return r
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// A process that cannot reach its only datastore is not healthy. Reporting
	// otherwise would leave Fly routing traffic to something that can only
	// return errors.
	if err := s.store.Ping(ctx); err != nil {
		s.log.Error("healthz: database unreachable", "error", err)
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

func (s *Server) serveStaticFile(name, contentType string) http.HandlerFunc {
	body, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		panic(fmt.Sprintf("read static/%s: %v", name, err))
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		// The service worker must be revalidated or clients pin an old shell.
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(body)
	}
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		// /healthz is polled constantly; logging every hit buries everything else.
		if r.URL.Path == "/healthz" && ww.Status() < 400 {
			return
		}
		s.log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
