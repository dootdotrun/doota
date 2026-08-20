package web

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/config"
	"github.com/dootdotrun/doot-ai/internal/store"
)

const minPasswordLen = 6

type settingsField struct {
	Key     string
	Label   string
	Help    string
	Kind    string
	Value   string
	Checked bool
}

type settingsGroup struct {
	Name   string
	Fields []settingsField
}

type settingsData struct {
	Groups                 []settingsGroup
	Secrets                []config.Secret
	Username               string
	PasswordChangeRequired bool
	SessionDays            int
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)

	cfg, err := s.store.LoadConfig(r.Context())
	if err != nil {
		s.log.Error("load config", "error", err)
		s.render(w, r, "settings", page{
			Title: "Settings", Active: "settings", User: user,
			Error: "Could not load configuration.",
		})
		return
	}

	s.render(w, r, "settings", page{
		Title:  "Settings",
		Active: "settings",
		Status: "configuration",
		User:   user,
		Notice: r.URL.Query().Get("notice"),
		Data: settingsData{
			Groups:                 buildGroups(cfg),
			Secrets:                s.cfg.Secrets(),
			Username:               user.Username,
			PasswordChangeRequired: user.PasswordChangeRequired,
			SessionDays:            int(s.sess.TTL().Hours() / 24),
		},
	})
}

func buildGroups(cfg store.AppConfig) []settingsGroup {
	groups := make([]settingsGroup, 0, len(store.ConfigGroups))
	for _, name := range store.ConfigGroups {
		g := settingsGroup{Name: name}
		for _, f := range store.ConfigFields {
			if f.Group != name {
				continue
			}
			g.Fields = append(g.Fields, settingsField{
				Key:     f.Key,
				Label:   f.Label,
				Help:    f.Help,
				Kind:    string(f.Kind),
				Value:   cfg.Display(f),
				Checked: f.Kind == store.KindBool && cfg.Bool(f.Key),
			})
		}
		if len(g.Fields) > 0 {
			groups = append(groups, g)
		}
	}
	return groups
}

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.settingsStatus(w, false, "Malformed form submission.")
		return
	}

	// An unchecked checkbox submits nothing, so absence has to mean false. That
	// is only safe when the whole form was submitted, otherwise a partial post
	// would silently switch every boolean off. This marker proves it.
	if !r.PostForm.Has("_config_form") {
		s.settingsStatus(w, false, "Incomplete form submission.")
		return
	}

	values := make(map[string]any, len(store.ConfigFields))
	for _, f := range store.ConfigFields {
		if f.Kind == store.KindBool {
			values[f.Key] = r.PostForm.Has(f.Key)
			continue
		}
		if !r.PostForm.Has(f.Key) {
			continue
		}
		parsed, err := store.ParseValue(f, r.PostFormValue(f.Key))
		if err != nil {
			s.settingsStatus(w, false, err.Error())
			return
		}
		values[f.Key] = parsed
	}

	if err := s.store.SetConfigValues(r.Context(), values); err != nil {
		s.log.Error("save config", "error", err)
		s.settingsStatus(w, false, "Could not save configuration.")
		return
	}

	s.log.Info("configuration saved", "keys", len(values))
	s.settingsStatus(w, true, "Saved.")
}

func (s *Server) handleSaveCredentials(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)

	if err := r.ParseForm(); err != nil {
		s.redirectSettings(w, r, "Malformed form submission.")
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("password_confirm")

	if username == "" {
		s.redirectSettings(w, r, "Username cannot be empty.")
		return
	}

	// A blank password field means "leave the password alone", so changing only
	// the username does not require retyping it.
	hash := user.PasswordHash
	passwordChanged := password != ""

	if passwordChanged {
		if len(password) < minPasswordLen {
			s.redirectSettings(w, r, fmt.Sprintf("Password must be at least %d characters.", minPasswordLen))
			return
		}
		if password != confirm {
			s.redirectSettings(w, r, "Passwords do not match.")
			return
		}
		var err error
		hash, err = store.HashPassword(password)
		if err != nil {
			s.log.Error("hash password", "error", err)
			s.redirectSettings(w, r, "Could not update credentials.")
			return
		}
	}

	// Only clear the change-required marker when the password actually changed;
	// renaming the account while still on the default password should keep it.
	if err := s.store.UpdateCredentials(r.Context(), user.ID, username, hash, passwordChanged); err != nil {
		s.log.Error("update credentials", "error", err)
		s.redirectSettings(w, r, "Could not update credentials. Is that username taken?")
		return
	}

	// Re-issue so the cookie is fresh; the user id is unchanged, but a password
	// change is a natural point to restart the session clock.
	s.sess.Issue(w, r, user.ID)

	s.log.Info("credentials updated", "username", username, "password_changed", passwordChanged)
	if passwordChanged {
		s.redirectSettings(w, r, "Credentials updated.")
		return
	}
	s.redirectSettings(w, r, "Username updated.")
}

func (s *Server) redirectSettings(w http.ResponseWriter, r *http.Request, notice string) {
	http.Redirect(w, r, "/settings?notice="+template.URLQueryEscaper(notice), http.StatusSeeOther)
}

// settingsStatus returns the htmx fragment that replaces the save status line.
func (s *Server) settingsStatus(w http.ResponseWriter, ok bool, msg string) {
	class := "status-ok"
	if !ok {
		class = "status-err"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !ok {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	fmt.Fprintf(w, `<p id="config-status" class="%s">%s</p>`, class, template.HTMLEscapeString(msg))
}
