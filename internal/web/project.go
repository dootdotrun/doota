package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/events"
	"github.com/dootdotrun/doot-ai/internal/project"
	"github.com/dootdotrun/doot-ai/internal/store"
)

// defaultPreviewPort is what a new project starts with. Changeable afterwards.
const defaultPreviewPort = 3000

type projectData struct {
	Project      *store.Project
	ProviderKind string
	Preview      bool
	WorkBranch   string
	RepoPath     string
	StatusLabel  string
	StatusClass  string
	Provisioning bool
	Log          string
}

func (s *Server) projectView(r *http.Request) (projectData, error) {
	p, err := s.projects.Active(r.Context())
	if err != nil {
		return projectData{}, err
	}

	// Reconcile with the provider so a sandbox that went to sleep or vanished is
	// reported honestly rather than from a stale row.
	s.projects.RefreshStatus(r.Context(), p)

	d := projectData{
		Project:      p,
		ProviderKind: s.projects.ProviderKind(),
		Preview:      previewAvailable(p),
		WorkBranch:   store.WorkBranch,
		RepoPath:     project.RepoPath,
		Provisioning: p.IsProvisioning(),
		Log:          p.Log(),
	}
	d.StatusLabel, d.StatusClass = statusPresentation(p.SandboxStatus)

	return d, nil
}

func statusPresentation(status string) (label, class string) {
	switch status {
	case store.SandboxProvisioning:
		return "setting up", "pill-warn"
	case store.SandboxReady:
		return "ready", "pill-ok"
	case store.SandboxSleeping:
		return "asleep", "pill-idle"
	case store.SandboxError:
		return "error", "pill-err"
	case store.SandboxMissing:
		return "sandbox missing", "pill-err"
	default:
		return status, "pill-idle"
	}
}

// handleProjectStatus returns the status card for htmx polling.
//
// The returned fragment carries the polling trigger only while provisioning, so
// polling stops by itself once setup finishes.
func (s *Server) handleProjectStatus(w http.ResponseWriter, r *http.Request) {
	d, err := s.projectView(r)
	if err != nil {
		// The project vanished from under the poll. Send the browser to a full
		// reload rather than swapping in an error fragment.
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	s.renderFragment(w, "project_status", d)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectProject(w, r, "", "Malformed form submission.")
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	repoURL := strings.TrimSpace(r.PostFormValue("repo_url"))

	p, err := s.projects.Create(r.Context(), name, repoURL, defaultPreviewPort)
	if errors.Is(err, store.ErrProjectExists) {
		s.redirectProject(w, r, "", "A project already exists. Delete it before creating another.")
		return
	}
	if err != nil {
		s.redirectProject(w, r, "", err.Error())
		return
	}

	s.log.Info("project create requested", "project_id", p.ID)
	s.redirectProject(w, r, "Setting up "+p.Name+".", "")
}

func (s *Server) handleProjectWake(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireProject(w, r)
	if !ok {
		return
	}
	if err := s.projects.Wake(r.Context(), p); err != nil {
		s.log.Error("wake sandbox", "error", err)
		s.redirectProject(w, r, "", "Could not wake the sandbox: "+err.Error())
		return
	}
	s.redirectProject(w, r, "Sandbox is awake.", "")
}

func (s *Server) handleProjectRecreate(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireProject(w, r)
	if !ok {
		return
	}
	if err := s.projects.Recreate(r.Context(), p); err != nil {
		s.log.Error("recreate sandbox", "error", err)
		s.redirectProject(w, r, "", "Could not recreate the sandbox: "+err.Error())
		return
	}
	s.redirectProject(w, r, "Rebuilding the sandbox from scratch.", "")
}

func (s *Server) handleProjectPreviewPort(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireProject(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectProject(w, r, "", "Malformed form submission.")
		return
	}

	port, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("preview_port")))
	if err != nil {
		s.redirectProject(w, r, "", "Preview port must be a number.")
		return
	}
	if err := s.projects.SetPreviewPort(r.Context(), p, port); err != nil {
		s.redirectProject(w, r, "", err.Error())
		return
	}
	s.redirectProject(w, r, "Preview port set to "+strconv.Itoa(port)+".", "")
}

func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireProject(w, r)
	if !ok {
		return
	}
	if err := s.projects.Delete(r.Context(), p); err != nil {
		s.log.Error("delete project", "error", err)
		s.redirectProject(w, r, "", "Could not delete the project: "+err.Error())
		return
	}
	s.redirectProject(w, r, "Project deleted. Create another below.", "")
}

func (s *Server) requireProject(w http.ResponseWriter, r *http.Request) (*store.Project, bool) {
	p, err := s.projects.Active(r.Context())
	if err != nil {
		s.redirectProject(w, r, "", "There is no project.")
		return nil, false
	}
	return p, true
}

func (s *Server) redirectProject(w http.ResponseWriter, r *http.Request, notice, errMsg string) {
	target := appPrefix + "/settings"
	switch {
	case errMsg != "":
		target += "?error=" + urlQueryEscape(errMsg)
	case notice != "":
		target += "?notice=" + urlQueryEscape(notice)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleProjectClearConversation deliberately touches only Postgres conversation
// state. It never reaches the project service, so sandbox files and processes are
// unaffected. An active runner is rejected to avoid deleting its live model
// context between an assistant tool request and its tool result.
func (s *Server) handleProjectClearConversation(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireProject(w, r)
	if !ok {
		return
	}
	if _, err := s.agent.ActiveRun(r.Context(), p.ID); err == nil {
		s.redirectChat(w, r, "", "Pause or finish the active run before clearing its conversation.")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.redirectChat(w, r, "", "Could not check the active run: "+err.Error())
		return
	}

	counts, err := s.store.ClearConversation(r.Context(), p.ID)
	if errors.Is(err, store.ErrActiveRun) {
		s.redirectChat(w, r, "", "Pause or finish the active run before clearing its conversation.")
		return
	}
	if err != nil {
		s.log.Error("clear conversation", "error", err)
		s.redirectChat(w, r, "", "Could not clear the conversation: "+err.Error())
		return
	}
	if counts.Empty() {
		s.redirectChat(w, r, "There was nothing to clear.", "")
		return
	}

	s.log.Info("conversation cleared",
		"messages", counts.Messages, "runs", counts.Runs,
		"tool_calls", counts.ToolCalls, "events", counts.Events)

	// Recorded after the delete, so this row is the first in a table the clear just
	// emptied. It is also what tells an open Chat screen to reload.
	if event, eventErr := s.store.AppendEvent(r.Context(), p.ID, "", store.EventConversationCleared,
		map[string]any{"message_count": counts.Messages, "run_count": counts.Runs}); eventErr == nil {
		s.events.Publish(events.Frame{ID: event.ID, Type: event.Type, Data: event.Payload})
	} else {
		s.log.Error("record cleared conversation event", "error", eventErr)
	}

	// Back to Chat, not Settings: clearing is reachable from the header on every
	// screen, and the result of the action is the empty conversation.
	s.redirectChat(w, r, fmt.Sprintf(
		"Deleted %d messages and %d runs. The sandbox was not touched.",
		counts.Messages, counts.Runs), "")
}
