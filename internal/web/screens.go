package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dootdotrun/doot-ai/internal/project"
	"github.com/dootdotrun/doot-ai/internal/sandbox"
	"github.com/dootdotrun/doot-ai/internal/store"
)

type taskView struct {
	N               int
	Summary, Status string
	Note            string
	Current         bool
}

type planData struct {
	Project                   *store.Project
	Run                       *store.Run
	Title, Status, BaseCommit string
	Feedback, DiffURL         string
	Tasks                     []taskView
	Empty, CanApprove         bool
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	d := planData{Empty: true}
	p, err := s.projects.Active(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		s.render(w, r, "plan", page{Title: "Board", Active: "plan", Status: "no project", User: userFrom(r), Data: d})
		return
	}
	if err != nil {
		s.render(w, r, "plan", page{Title: "Board", Active: "plan", Status: "unavailable", User: userFrom(r),
			Error: "Could not load the project.", Data: d})
		return
	}
	d.Project = p
	if run, runErr := s.agent.ActiveRun(r.Context(), p.ID); runErr == nil {
		d.Run = run
	}

	pad, err := s.store.Scratchpad(r.Context(), p.ID)
	if err != nil {
		s.log.Error("plan: read scratchpad", "error", err)
		s.render(w, r, "plan", page{Title: "Board", Active: "plan", Status: "unavailable", User: userFrom(r),
			Error: "Could not load the task board.", Data: d})
		return
	}
	d.Feedback = pad.Feedback
	d.Empty = pad.Empty()
	if !d.Empty {
		d.Title, d.Status, d.BaseCommit = pad.Title, pad.Status, pad.BaseCommit
		current := pad.Current()
		for _, t := range pad.Tasks {
			d.Tasks = append(d.Tasks, taskView{N: t.N, Summary: t.Summary, Status: t.Status, Note: t.Note,
				Current: current != nil && current.N == t.N})
		}
		if pad.BaseCommit != "" {
			d.DiffURL = "/plan/diff"
		}
		d.CanApprove = d.Run != nil && d.Run.State == store.RunAwaitingHuman &&
			d.Run.Awaiting() == store.AwaitingPlanApproval && pad.AwaitingApproval()
	}

	status := "no plan"
	if !d.Empty {
		status = pad.Status
	}
	s.render(w, r, "plan", page{Title: "Board", Active: "plan", Status: status, User: userFrom(r),
		Notice: r.URL.Query().Get("notice"), Error: r.URL.Query().Get("error"), Data: d})
}

func (s *Server) handlePlanApprove(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireProjectForPlan(w, r)
	if !ok {
		return
	}
	if err := s.agent.ApprovePlan(r.Context(), p); err != nil {
		s.redirectPlan(w, r, "", "Could not approve: "+err.Error())
		return
	}
	s.redirectPlan(w, r, "Plan approved. Starting the first subtask.", "")
}

func (s *Server) handlePlanRevise(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireProjectForPlan(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectPlan(w, r, "", "Malformed request.")
		return
	}
	feedback := strings.TrimSpace(r.PostFormValue("feedback"))
	if feedback == "" {
		s.redirectPlan(w, r, "", "Revision feedback is required.")
		return
	}
	if err := s.agent.RevisePlan(r.Context(), p.ID, feedback); err != nil {
		s.redirectPlan(w, r, "", "Could not request a revision: "+err.Error())
		return
	}
	s.redirectPlan(w, r, "Feedback sent. Doot will present a new plan.", "")
}

// handlePlanDiff returns the diff since the plan was approved.
func (s *Server) handlePlanDiff(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireProjectForPlan(w, r)
	if !ok {
		return
	}
	pad, err := s.store.Scratchpad(r.Context(), p.ID)
	if err != nil || pad.BaseCommit == "" {
		http.NotFound(w, r)
		return
	}
	sb, err := s.projects.Sandbox(r.Context(), p)
	if err != nil {
		http.Error(w, "sandbox unavailable", http.StatusServiceUnavailable)
		return
	}
	res, err := sb.Exec(r.Context(), sandbox.Command{
		Cmd:     "git --no-pager diff " + shellQuoteArg(pad.BaseCommit),
		Dir:     project.RepoPath,
		Timeout: 2 * time.Minute,
	})
	if err != nil || res.ExitCode != 0 {
		http.Error(w, fmt.Sprintf("could not generate the diff: %v %s", err, strings.TrimSpace(res.Output())),
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(res.Output()))
}

func (s *Server) requireProjectForPlan(w http.ResponseWriter, r *http.Request) (*store.Project, bool) {
	p, err := s.projects.Active(r.Context())
	if err != nil {
		s.redirectPlan(w, r, "", "There is no project yet.")
		return nil, false
	}
	return p, true
}

func (s *Server) redirectPlan(w http.ResponseWriter, r *http.Request, notice, errorMessage string) {
	target := "/plan"
	if errorMessage != "" {
		target += "?error=" + urlQueryEscape(errorMessage)
	} else if notice != "" {
		target += "?notice=" + urlQueryEscape(notice)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func shellQuoteArg(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

type activityData struct {
	Project    *store.Project
	Archived   []archiveMessage
	ToolCalls  []activityToolCall
	ToolName   string
	ErrorsOnly bool
}

type archiveMessage struct {
	Role, Kind, Content, At string
}

type activityToolCall struct {
	Name, Content, At, Duration string
	IsError                     bool
}

// archivedLimit bounds the cleared-history list on Activity.
const archivedLimit = 200

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	projectRow, err := s.store.MostRecentProject(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		s.render(w, r, "activity", page{Title: "Activity", Active: "activity", Status: "nothing yet",
			User: userFrom(r), Data: activityData{}})
		return
	}
	if err != nil {
		s.log.Error("activity: load project", "error", err)
		s.render(w, r, "activity", page{Title: "Activity", Active: "activity", Status: "unavailable",
			User: userFrom(r), Error: "Could not load history.", Data: activityData{}})
		return
	}
	d := activityData{Project: projectRow, ToolName: strings.TrimSpace(r.URL.Query().Get("tool")),
		ErrorsOnly: r.URL.Query().Get("errors") == "1"}
	if messages, err := s.store.ArchivedMessages(r.Context(), projectRow.ID, archivedLimit); err == nil {
		for _, message := range messages {
			d.Archived = append(d.Archived, archiveMessage{Role: message.Role, Kind: message.MessageKind(),
				Content: message.Content, At: message.CreatedAt.UTC().Format("2 Jan 15:04")})
		}
	} else {
		s.log.Error("activity: archived messages", "error", err)
	}
	if calls, err := s.store.ProjectToolCalls(r.Context(), projectRow.ID, d.ToolName, d.ErrorsOnly, 100); err == nil {
		for _, call := range calls {
			d.ToolCalls = append(d.ToolCalls, activityToolCall{Name: call.ToolName, Content: call.Content(),
				IsError: call.IsError, At: call.CreatedAt.UTC().Format("2 Jan 15:04"),
				Duration: call.Duration().Round(time.Millisecond).String()})
		}
	} else {
		s.log.Error("activity: tool calls", "error", err)
	}
	status := "history"
	if projectRow.DeletedAt.Valid {
		status = "archived project"
	}
	s.render(w, r, "activity", page{Title: "Activity", Active: "activity", Status: status, User: userFrom(r), Data: d})
}
