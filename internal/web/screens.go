package web

import (
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

// planView is the task board as the Chat screen renders it.
type planView struct {
	Title, Status, BaseCommit string
	Feedback, DiffURL         string
	Tasks                     []taskView
	Current                   *taskView
	Done, Total               int
	CanApprove                bool
}

// loadPlan builds the plan view, or nil when there is nothing planned.
//
// Errors are logged and swallowed: the plan is a panel on the Chat screen now, and
// failing to read the scratchpad should cost the panel, not the conversation.
func (s *Server) loadPlan(r *http.Request, p *store.Project, run *store.Run) *planView {
	pad, err := s.store.Scratchpad(r.Context(), p.ID)
	if err != nil {
		s.log.Error("chat: read scratchpad", "error", err)
		return nil
	}
	if pad.Empty() {
		return nil
	}

	v := &planView{
		Title: pad.Title, Status: pad.Status, BaseCommit: pad.BaseCommit,
		Feedback: pad.Feedback, Total: len(pad.Tasks),
	}
	current := pad.Current()
	for _, t := range pad.Tasks {
		task := taskView{N: t.N, Summary: t.Summary, Status: t.Status, Note: t.Note,
			Current: current != nil && current.N == t.N}
		if t.Status == "complete" {
			v.Done++
		}
		v.Tasks = append(v.Tasks, task)
		if task.Current {
			v.Current = &v.Tasks[len(v.Tasks)-1]
		}
	}
	if pad.BaseCommit != "" {
		v.DiffURL = appPrefix + "/plan/diff"
	}
	v.CanApprove = run != nil && run.State == store.RunAwaitingHuman &&
		run.Awaiting() == store.AwaitingPlanApproval && pad.AwaitingApproval()
	return v
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
	s.redirectPlan(w, r, "Plan approved. Starting the first task.", "")
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

// redirectPlan returns to the Chat screen, which is where the plan is rendered.
func (s *Server) redirectPlan(w http.ResponseWriter, r *http.Request, notice, errorMessage string) {
	target := appPrefix + "/"
	if errorMessage != "" {
		target += "?error=" + urlQueryEscape(errorMessage)
	} else if notice != "" {
		target += "?notice=" + urlQueryEscape(notice)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func shellQuoteArg(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }
