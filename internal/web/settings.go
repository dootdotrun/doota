package web

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/store"
)

const minPasswordLen = 6

// maxNotesLen matches the cap the remember and record_orientation tools enforce on
// the agent, so the operator cannot save something the agent would have been refused.
const maxNotesLen = 8000

// clearSuffix names the companion checkbox that empties a stored secret.
const clearSuffix = "__clear"

type settingsField struct {
	Key     string
	Label   string
	Help    string
	Kind    string
	Value   string
	Checked bool
	Options []settingsOption

	// Secret fields render an empty input whatever is stored. IsSet is the only
	// thing said about the stored value, and Placeholder says what submitting
	// nothing will do.
	Secret bool
	IsSet  bool
}

// Placeholder is the hint shown inside an empty secret input.
func (f settingsField) Placeholder() string {
	if f.IsSet {
		return "stored — leave blank to keep"
	}
	return "not set"
}

// ClearKey names the checkbox that removes a stored secret.
func (f settingsField) ClearKey() string { return f.Key + clearSuffix }

// settingsOption is one entry of a KindChoice select.
type settingsOption struct {
	Value    string
	Label    string
	Selected bool
}

type settingsGroup struct {
	Name   string
	Fields []settingsField

	// Collapsed starts the group closed, which is the default for every group.
	// Fields inside a closed <details> still submit with the form.
	Collapsed bool

	// NeedsAttention marks a group containing a required credential that is not set.
	// It is the only thing that opens a group on arrival.
	NeedsAttention bool
}

type settingsData struct {
	Groups                 []settingsGroup
	Username               string
	PasswordChangeRequired bool
	SessionDays            int
	Missing                []string
	Provider               string

	// Memories is the agent's durable memory, editable here.
	//
	// It had no read path anywhere in the web layer at all: the agent rewrote it
	// through the remember tool, it was injected into the system prompt on every
	// call, and the operator could neither see it nor correct it. A drifted or
	// wrong line therefore shaped every future turn invisibly and permanently.
	Memories    string
	HasMemories bool

	// Orientation is what the agent worked out about the repository. Editable for the
	// same reason memories are: the agent writes it, it shapes every turn, and a
	// stale build command silently misdirects every future conversation.
	Orientation string

	// Project is the section at the bottom of the screen, which used to be a tab
	// of its own. Always populated — with a zero value carrying only the provider
	// name when there is no project, so the template can render the create form.
	Project projectData
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)

	// Read first, so the header's action icons and the Project section below agree
	// with each other even if the config read fails.
	pv, projectErr := s.projectView(r)
	switch {
	case errors.Is(projectErr, store.ErrNotFound):
		pv = projectData{ProviderKind: s.projects.ProviderKind()}
	case projectErr != nil:
		s.log.Error("settings: load project", "error", projectErr)
		pv = projectData{ProviderKind: s.projects.ProviderKind()}
	}

	p := page{
		Title:      "Settings",
		Active:     "settings",
		Status:     pv.StatusLabel,
		User:       user,
		Notice:     r.URL.Query().Get("notice"),
		Error:      r.URL.Query().Get("error"),
		HasProject: pv.Project != nil,
		Preview:    pv.Preview,
	}

	// Data must be a settingsData on every path, including the failure below: the
	// template dereferences it on its first line, so a nil Data would turn "could
	// not load configuration" into a bare 500.
	d := settingsData{
		Username: user.Username,
		Provider: s.projects.ProviderKind(),
		Project:  pv,
	}

	cfg, err := s.store.LoadConfig(r.Context())
	if err != nil {
		s.log.Error("load config", "error", err)
		p.Error = "Could not load configuration."
		p.Data = d
		s.render(w, r, "settings", p)
		return
	}

	// Only meaningful with a project, since memories are stored per project.
	if pv.Project != nil {
		d.HasMemories = true
		if memories, memErr := s.store.Memories(r.Context(), pv.Project.ID); memErr == nil {
			d.Memories = memories
		} else if !errors.Is(memErr, store.ErrNotFound) {
			s.log.Error("settings: load memories", "error", memErr)
		}
		if notes, notesErr := s.store.Orientation(r.Context(), pv.Project.ID); notesErr == nil {
			d.Orientation = notes
		} else if !errors.Is(notesErr, store.ErrNotFound) {
			s.log.Error("settings: load orientation", "error", notesErr)
		}
	}

	d.Groups = buildGroups(cfg)
	d.PasswordChangeRequired = user.PasswordChangeRequired
	d.SessionDays = int(s.sess.TTL().Hours() / 24)
	d.Missing = cfg.MissingCredentials()
	p.Data = d

	s.render(w, r, "settings", p)
}

func buildGroups(cfg store.AppConfig) []settingsGroup {
	groups := make([]settingsGroup, 0, len(store.ConfigGroups))
	for _, name := range store.ConfigGroups {
		g := settingsGroup{Name: name}
		for _, f := range store.ConfigFields {
			if f.Group != name {
				continue
			}
			current := cfg.Display(f)
			var options []settingsOption
			for _, c := range f.Choices {
				label := c
				if c == "" {
					// The blank option needs a name, or it renders as an empty row that
					// looks like a rendering bug rather than a deliberate default.
					label = "let the model decide"
				}
				options = append(options, settingsOption{Value: c, Label: label, Selected: c == current})
			}

			field := settingsField{
				Key:     f.Key,
				Label:   f.Label,
				Help:    f.Help,
				Kind:    string(f.Kind),
				Value:   current,
				Checked: f.Kind == store.KindBool && cfg.Bool(f.Key),
				Options: options,
				Secret:  f.Secret(),
				IsSet:   cfg.IsSet(f.Key),
			}
			if !field.IsSet && store.RequiredCredential(f.Key) {
				g.NeedsAttention = true
			}
			g.Fields = append(g.Fields, field)
		}
		if len(g.Fields) > 0 {
			// Everything starts closed, so opening a group is something the operator
			// chose to do.
			//
			// The rule used to be "collapsed when every field is a textarea", which
			// meant Credentials, Model and Git could never collapse — three of the five
			// groups, plus four more sections below them, all open at once. Arriving at
			// Settings to change one value meant reading the entire configuration
			// surface of the application.
			//
			// The single exception is a group holding a credential that is missing,
			// because the banner at the top of the screen sends the operator here to
			// fill it in and hiding it behind a click would be a dead end.
			g.Collapsed = !g.NeedsAttention
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
		raw := r.PostFormValue(f.Key)

		if f.Secret() && strings.TrimSpace(raw) == "" {
			// Blank means "keep", because a secret input is always rendered empty and
			// an empty submission therefore carries no information. Without this,
			// saving the form to change the model name would erase every credential.
			//
			// Which leaves no way to remove a revoked token, so clearing is an
			// explicit checkbox rather than an overloaded empty string.
			if r.PostForm.Has(f.Key + clearSuffix) {
				values[f.Key] = ""
			}
			continue
		}

		parsed, err := store.ParseValue(f, raw)
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

// handleResetConfigField restores one setting to its compiled-in default.
//
// This exists because EnsureConfigDefaults seeds rows with ON CONFLICT DO NOTHING,
// so a key written once is never updated by a later deploy. That is the right rule
// — it stops a redeploy reverting an edit — but it means a default that shipped
// broken stays broken forever. The setup script shipped with bash arrays that dash
// could not parse, and without this the only remedy was retyping sixty lines of
// shell into a phone.
//
// Deliberately not a "reset everything" button: one field at a time, named, and
// confirmed in the template.
func (s *Server) handleResetConfigField(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.resetStatus(w, "Malformed form submission.")
		return
	}

	key := r.PostFormValue("key")
	field, ok := store.FieldByKey(key)
	if !ok {
		s.resetStatus(w, "No such setting.")
		return
	}
	// Every credential's default is the empty string, so "reset" would read as an
	// accidental wipe. Clearing one is already an explicit checkbox on the form.
	if field.Secret() {
		s.resetStatus(w, "Credentials have no default to reset to.")
		return
	}

	if err := s.store.SetConfigValues(r.Context(), map[string]any{key: field.Default}); err != nil {
		s.log.Error("reset config field", "error", err, "key", key)
		s.resetStatus(w, "Could not reset "+field.Label+".")
		return
	}

	s.log.Info("configuration reset to default", "key", key)
	// A full reload rather than a status line: the whole point is the restored
	// contents of the textarea, which cannot be swapped in from here.
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
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

// handleSaveMemories overwrites the agent's durable memory with the operator's
// edit.
//
// Replace, not merge, which is the same contract the remember tool already has: the
// agent is shown the whole text and returns the whole text. An empty submission is
// a legitimate "forget all of this" rather than a mistake, so it is not rejected.
func (s *Server) handleSaveMemories(w http.ResponseWriter, r *http.Request) {
	p, err := s.projects.Active(r.Context())
	if err != nil {
		s.redirectSettings(w, r, "There is no project to store memories for.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectSettings(w, r, "Malformed form submission.")
		return
	}

	// One handler for both durable notes, because the form posts whichever it owns
	// and the validation and failure modes are identical.
	if raw, present := r.PostForm["memories"]; present {
		memories := raw[0]
		if len(memories) > maxNotesLen {
			s.redirectSettings(w, r, fmt.Sprintf("Memories must be under %d characters.", maxNotesLen))
			return
		}
		if err := s.store.SetMemories(r.Context(), p.ID, memories); err != nil {
			s.log.Error("save memories", "error", err)
			s.redirectSettings(w, r, "Could not save memories.")
			return
		}
		s.log.Info("memories edited by the operator", "chars", len(strings.TrimSpace(memories)))
		s.redirectSettings(w, r, "Memories saved.")
		return
	}

	if raw, present := r.PostForm["orientation"]; present {
		notes := raw[0]
		if len(notes) > maxNotesLen {
			s.redirectSettings(w, r, fmt.Sprintf("Orientation must be under %d characters.", maxNotesLen))
			return
		}
		if err := s.store.SetOrientation(r.Context(), p.ID, notes); err != nil {
			s.log.Error("save orientation", "error", err)
			s.redirectSettings(w, r, "Could not save orientation.")
			return
		}
		s.log.Info("orientation edited by the operator", "chars", len(strings.TrimSpace(notes)))
		s.redirectSettings(w, r, "Orientation saved.")
		return
	}

	s.redirectSettings(w, r, "Nothing to save.")
}

func (s *Server) redirectSettings(w http.ResponseWriter, r *http.Request, notice string) {
	http.Redirect(w, r, appPrefix+"/settings?notice="+template.URLQueryEscaper(notice), http.StatusSeeOther)
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

// resetStatus reports a failed reset.
//
// Separate from settingsStatus because it answers 200. htmx does not swap a 4xx
// response by default, so the 422 that settingsStatus returns would leave the
// reset button silently doing nothing at all.
func (s *Server) resetStatus(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<p id="config-status" class="status-err">%s</p>`, template.HTMLEscapeString(msg))
}
