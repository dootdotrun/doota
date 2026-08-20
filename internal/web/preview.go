package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/dootdotrun/doot-ai/internal/store"
)

// previewPrefix is where the sandbox's dev server is mounted.
const previewPrefix = "/preview"

// handlePreview reverse-proxies to a dev server running inside the sandbox.
//
// Traffic goes phone -> this app -> a TCP tunnel into the sandbox -> the dev
// server. Two things fall out of doing it this way rather than handing out the
// sandbox's own public URL:
//
//   - Preview access is gated by the app's session, so there is no secret URL to
//     leak and no provider token in the browser.
//   - Any port works. The Sprite's own public URL only routes to 8080; dialling
//     the port directly makes that irrelevant for the human preview path.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	p, err := s.projects.Active(r.Context())
	if err != nil {
		s.previewError(w, r, "No project", "Create a project before opening a preview.")
		return
	}
	if p.Sandbox() == "" {
		s.previewError(w, r, "No sandbox", "This project has no sandbox yet.")
		return
	}

	sb, err := s.projects.Sandbox(r.Context(), p)
	if err != nil {
		s.previewError(w, r, "Sandbox unavailable", err.Error())
		return
	}

	port := p.PreviewPort
	target, err := url.Parse("http://sandbox.invalid")
	if err != nil {
		s.previewError(w, r, "Preview failed", err.Error())
		return
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Strip the mount prefix so the app sees the paths it expects.
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, previewPrefix)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport: &http.Transport{
			// Every connection is a tunnel into the sandbox, so the dialler
			// ignores the address entirely and dials the configured port.
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return sb.DialPort(ctx, port)
			},
			// Connections are per-request tunnels; pooling them across requests
			// would outlive the sandbox handle they were opened with.
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 60 * time.Second,
		},
		// Streaming responses and server-sent events from the previewed app
		// should not be buffered.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Warn("preview proxy", "error", err, "port", port, "path", r.URL.Path)
			s.previewError(w, r, "Nothing listening",
				fmt.Sprintf("Could not reach a server on port %d inside the sandbox. "+
					"Start a dev server there, or change the preview port on the Project screen.", port))
		},
	}

	proxy.ServeHTTP(w, r)
}

// previewError renders a readable page instead of a bare proxy error.
//
// A blank 502 in a phone browser is indistinguishable from the app being broken;
// naming the port and the likely cause is the difference between a dead end and
// an obvious next step.
func (s *Server) previewError(w http.ResponseWriter, r *http.Request, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadGateway)

	// Deliberately self-contained: the previewed app is a different origin in
	// spirit, and pulling in the app shell here would be misleading.
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<link rel="stylesheet" href="/static/app.css"></head>
<body class="centered"><main class="gate">
<h1 class="gate-mark">%s</h1>
<p class="muted">%s</p>
<p><a href="/project">Back to Project</a></p>
</main></body></html>`,
		htmlEscape(title), htmlEscape(title), htmlEscape(detail))
}

// handlePreviewRedirect sends a bare /preview to /preview/ so relative asset
// paths in the previewed app resolve correctly.
func (s *Server) handlePreviewRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, previewPrefix+"/", http.StatusMovedPermanently)
}

// previewAvailable reports whether a preview link is worth showing.
func previewAvailable(p *store.Project) bool {
	return p != nil && p.Sandbox() != "" &&
		(p.SandboxStatus == store.SandboxReady || p.SandboxStatus == store.SandboxSleeping)
}
