package web

import (
	"bytes"
	"html/template"
	"net/http"
	"net/url"

	"github.com/dootdotrun/doot-ai/internal/store"
)

// page is the data every template receives.
type page struct {
	Title  string
	Active string // which header link is highlighted
	Status string // status chip text
	User   *store.User
	Notice string
	Error  string
	Data   any

	// State the header's action buttons need. The header is part of the shell, so
	// whether clearing and previewing are possible has to be known on every screen
	// rather than only on the one that owns the project.
	HasProject bool
	Preview    bool
}

// htmlEscape escapes text for interpolation into hand-written HTML.
func htmlEscape(s string) string { return template.HTMLEscapeString(s) }

// urlQueryEscape escapes text for a query-string value.
func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// renderFragment writes a partial for htmx to swap in.
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	t, ok := s.fragments[name]
	if !ok {
		s.log.Error("unknown fragment", "name", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name+".html", data); err != nil {
		s.log.Error("render fragment", "name", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// render writes a page, buffering first so a template error produces a clean
// 500 rather than a half-written response the browser tries to parse.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p page) {
	t, ok := s.pages[name]
	if !ok {
		s.log.Error("unknown template", "name", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	root := "layout.html"
	if standalonePages[name] {
		root = name + ".html"
	}

	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, root, p); err != nil {
		s.log.Error("render template", "name", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}
