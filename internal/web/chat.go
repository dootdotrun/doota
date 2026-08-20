package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/agent"
	"github.com/dootdotrun/doot-ai/internal/store"
)

// transcriptLimit is how many recent turns the Chat screen renders.
//
// A goal produces hundreds of tool calls, and a phone rendering all of them is slow
// for no benefit — the interesting end of a conversation is the recent end. Older
// turns stay in Postgres and become browsable on Activity in Phase 8.
const transcriptLimit = 300

// messageView is one rendered turn.
type messageView struct {
	ID          int64
	Role        string
	Kind        string
	Content     string
	Interrupted bool
	At          string

	// HTML is Content rendered as markdown, and is set only for the model's own
	// prose. A user turn keeps Content and renders as preformatted text: the person
	// typing knows what they wrote, and silently reinterpreting a line that happens
	// to start with a `#` or a `-` is a worse surprise than not formatting it.
	HTML template.HTML

	// Tool-result fields.
	Tool    string
	Diff    *diffView
	Review  *reviewView
	Summary string
	IsError bool
}

type reviewView struct {
	Clean    bool
	Findings template.HTML
}

// diffView is a rendered file change.
type diffView struct {
	Path    string
	Added   int
	Removed int
	Lines   []diffLine
}

// diffLine is one line of a unified diff, tagged for colouring.
type diffLine struct {
	Kind string // add | del | meta | ctx
	Text string
}

type chatData struct {
	Project        *store.Project
	Messages       []messageView
	LastMessageID  int64
	Run            *store.Run
	AwaitingDetail string
	AwaitingError  string
	Ready          bool
	Blocked        string
	Model          string

	// Plan is the task board, rendered on this screen rather than its own.
	//
	// It used to be a separate tab, which meant the single most common interruption
	// — the agent stopping to ask whether a plan is right — was a decision you had
	// to leave the conversation to make. It is nil when there is no plan.
	Plan *planView

	// Setup names credentials that are not configured yet. Rendered as one line
	// with a link, because a fresh install otherwise looks broken rather than
	// unconfigured.
	Setup []string
}

// sandboxBlockedMessage explains an unready sandbox in a sentence.
//
// Written per status rather than interpolated, because the interpolated version
// produced "The sandbox is error." — and the four cases do not want the same
// sentence anyway: two of them are fixed by waiting and two need a button on the
// Project screen.
func sandboxBlockedMessage(status string) string {
	switch status {
	case store.SandboxProvisioning:
		return "The sandbox is still being set up. I can talk, but tools that touch it will fail until it is ready."
	case store.SandboxSleeping:
		return "The sandbox is asleep. Anything I run will wake it, which takes a moment."
	case store.SandboxError:
		return "The sandbox failed to set up. Check the setup log on the Project screen, then recreate it."
	case store.SandboxMissing:
		return "The sandbox no longer exists. Recreate it on the Project screen."
	default:
		return "The sandbox is not ready. I can talk, but tools that touch it will fail."
	}
}

func (d chatData) Busy() bool { return d.Run != nil && d.Run.State == store.RunRunning }
func (d chatData) CanSend() bool {
	return d.Project != nil && (d.Run == nil || (d.Run.State == store.RunAwaitingHuman && d.Run.Awaiting() == store.AwaitingQuestion))
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	d := chatData{}

	cfg, err := s.store.LoadConfig(r.Context())
	if err == nil {
		d.Model = cfg.Text(store.KeyModelName)
		d.Setup = cfg.MissingCredentials()
	}

	p, err := s.projects.Active(r.Context())
	switch {
	case errors.Is(err, store.ErrNotFound):
		d.Blocked = "There is no project yet. Create one on the Project screen and I can start working in it."
	case err != nil:
		s.log.Error("chat: load project", "error", err)
		d.Blocked = "Could not load the project."
	default:
		d.Project = p
		d.Ready = p.IsReady()
		if run, runErr := s.agent.ActiveRun(r.Context(), p.ID); runErr == nil {
			d.Run = run
			if run.State == store.RunAwaitingHuman && len(run.AwaitingPayload) > 0 {
				var payload map[string]any
				if json.Unmarshal(run.AwaitingPayload, &payload) == nil {
					if message, ok := payload["error"].(string); ok {
						d.AwaitingError = message
					}
					if action, ok := payload["action"].(string); ok {
						d.AwaitingDetail = action
					} else if question, ok := payload["question"].(string); ok {
						d.AwaitingDetail = question
					}
				}
			}
		} else if !errors.Is(runErr, store.ErrNotFound) {
			s.log.Error("chat: load run", "error", runErr)
		}
		if !p.IsReady() {
			d.Blocked = sandboxBlockedMessage(p.SandboxStatus)
		}

		d.Plan = s.loadPlan(r, p, d.Run)

		msgs, err := s.store.TranscriptMessages(r.Context(), p.ID, transcriptLimit)
		if err != nil {
			s.log.Error("chat: load transcript", "error", err)
		} else {
			d.Messages = renderMessages(msgs)
			if n := len(msgs); n > 0 {
				d.LastMessageID = msgs[n-1].ID
			}
		}
	}

	status := "idle"
	if d.Busy() {
		status = "working"
	} else if d.Run != nil {
		status = d.Run.State
	}

	s.render(w, r, "chat", page{
		Title:  "Chat",
		Active: "chat",
		Status: status,
		User:   userFrom(r),
		Notice: r.URL.Query().Get("notice"),
		Error:  r.URL.Query().Get("error"),
		Data:   d,
	})
}

// handleChatSend accepts a message and starts a turn.
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireProjectForChat(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectChat(w, r, "", "Malformed form submission.")
		return
	}

	text := strings.TrimSpace(r.PostFormValue("message"))
	if text == "" {
		s.redirectChat(w, r, "", "")
		return
	}

	if run, runErr := s.agent.ActiveRun(r.Context(), p.ID); runErr == nil && run.State == store.RunAwaitingHuman && run.Awaiting() == store.AwaitingPlanApproval {
		s.redirectChat(w, r, "Approve or reject the plan before sending another task.", "")
		return
	}

	if _, err := s.agent.Submit(r.Context(), p, text); err != nil {
		if errors.Is(err, agent.ErrBusy) {
			s.redirectChat(w, r, "", "I am still working on the last message. Pause it first if you need to intervene.")
			return
		}
		s.log.Error("chat: submit", "error", err)
		s.redirectChat(w, r, "", "Could not send: "+err.Error())
		return
	}

	// A redirect rather than a rendered fragment: the SSE stream is what fills the
	// transcript in, and reloading proves the page rebuilds from Postgres — which is
	// the property the whole streaming design depends on.
	s.redirectChat(w, r, "", "")
}

// handleChatPause stops the run: the model stream and any running command are
// cancelled, and the run parks at the last durable boundary.
func (s *Server) handleChatPause(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireProjectForChat(w, r)
	if !ok {
		return
	}
	err := s.agent.Pause(r.Context(), p.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.redirectChat(w, r, "Nothing was running.", "")
		return
	}
	if err != nil {
		s.log.Error("chat: pause", "error", err)
		s.redirectChat(w, r, "", "Could not pause: "+err.Error())
		return
	}
	s.redirectChat(w, r, "Stopped. Press Resume to carry on from the last saved point.", "")
}

func (s *Server) handleChatResume(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireProjectForChat(w, r)
	if !ok {
		return
	}
	run, runErr := s.agent.ActiveRun(r.Context(), p.ID)
	if runErr != nil {
		s.redirectChat(w, r, "", "There is no resumable run.")
		return
	}
	if run.State == store.RunAwaitingHuman && run.Awaiting() == store.AwaitingQuestion {
		s.redirectChat(w, r, "", "Reply in chat to answer this question and continue.")
		return
	}
	if run.State == store.RunAwaitingHuman && run.Awaiting() == store.AwaitingPlanApproval {
		s.redirectChat(w, r, "Approve or reject the plan on this screen before any work starts.", "")
		return
	}
	if err := s.agent.Resume(r.Context(), p.ID); err != nil {
		s.log.Error("chat: resume", "error", err)
		s.redirectChat(w, r, "", "Could not resume: "+err.Error())
		return
	}
	s.redirectChat(w, r, "Run resumed.", "")
}

// handleChatTail returns rendered messages newer than an id.
//
// The browser is told by SSE that something happened and comes here for the canonical
// HTML. That keeps one renderer on the server: the event stream cannot drift from
// what a page reload would produce, because a page reload runs the same code.
func (s *Server) handleChatTail(w http.ResponseWriter, r *http.Request) {
	p, err := s.projects.Active(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}

	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	msgs, err := s.store.MessagesAfter(r.Context(), p.ID, after)
	if err != nil {
		s.log.Error("chat: tail", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if n := len(msgs); n > 0 {
		// The client reads this to advance its cursor, so it never refetches or skips.
		w.Header().Set("X-Last-Message-Id", strconv.FormatInt(msgs[n-1].ID, 10))
	}
	s.renderFragment(w, "messages", renderMessages(msgs))
}

func (s *Server) requireProjectForChat(w http.ResponseWriter, r *http.Request) (*store.Project, bool) {
	p, err := s.projects.Active(r.Context())
	if err != nil {
		s.redirectChat(w, r, "", "There is no project yet.")
		return nil, false
	}
	return p, true
}

func (s *Server) redirectChat(w http.ResponseWriter, r *http.Request, notice, errMsg string) {
	target := "/"
	switch {
	case errMsg != "":
		target += "?error=" + urlQueryEscape(errMsg)
	case notice != "":
		target += "?notice=" + urlQueryEscape(notice)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// renderMessages turns stored rows into view models.
func renderMessages(msgs []*store.Message) []messageView {
	out := make([]messageView, 0, len(msgs))
	for _, m := range msgs {
		v := messageView{
			ID:          m.ID,
			Role:        m.Role,
			Kind:        m.MessageKind(),
			Content:     m.Content,
			Interrupted: m.Interrupted,
			At:          m.CreatedAt.UTC().Format("15:04"),
			Tool:        m.Tool(),
		}

		// An assistant turn whose only purpose was requesting tools has no text.
		// Rendering an empty bubble for it would be noise; the tool rows that follow
		// say everything it would have said.
		if m.Role == store.RoleAssistant && strings.TrimSpace(m.Content) == "" && m.HasToolCalls() {
			continue
		}

		if m.Role == store.RoleTool {
			v.Summary, v.IsError = summariseToolResult(m)
			v.Diff = extractDiff(m)
			v.Review = extractReview(m)
		} else if m.Role != store.RoleUser {
			// Everything the model wrote — assistant prose, a question from ask_human,
			// a notice the loop emitted — is markdown. A tool result is not: it is
			// command output, and belongs in a <pre> exactly as it arrived.
			v.HTML = renderMarkdown(m.Content)
		}

		out = append(out, v)
	}
	return out
}

// summariseToolResult produces the one-line collapsed form of a tool result.
//
// Tool calls collapse by default because a goal produces hundreds of them, and a
// transcript rendered expanded buries the reasoning the human is actually reading.
func summariseToolResult(m *store.Message) (summary string, isError bool) {
	content := strings.TrimSpace(m.Content)

	// The tool layer reports failure in the text rather than in a column, so the
	// heuristic is the first line — which every tool writes as a sentence about what
	// went wrong.
	first := content
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	if len(first) > 120 {
		first = first[:120] + " …"
	}

	if m.Display() {
		var d struct {
			Path    string `json:"path"`
			Added   int    `json:"added"`
			Removed int    `json:"removed"`
			Scope   string `json:"scope"`
			Status  int    `json:"status"`
			Name    string `json:"name"`
			Command string `json:"command"`
			Exit    *int   `json:"exit_code"`
			Matches *int   `json:"matches"`
		}
		if json.Unmarshal(m.ToolDisplay, &d) == nil {
			switch {
			case d.Path != "" && (d.Added > 0 || d.Removed > 0):
				return fmt.Sprintf("%s  +%d −%d", d.Path, d.Added, d.Removed), false
			case d.Path != "":
				return d.Path, false
			case d.Command != "":
				exit := 0
				if d.Exit != nil {
					exit = *d.Exit
				}
				return fmt.Sprintf("%s  (exit %d)", clip(d.Command, 70), exit), exit != 0
			case d.Status != 0:
				return fmt.Sprintf("HTTP %d", d.Status), d.Status >= 400
			case d.Matches != nil:
				return fmt.Sprintf("%d matches", *d.Matches), false
			case d.Name != "":
				return d.Name, false
			case d.Scope != "":
				return d.Scope, false
			}
		}
	}
	return first, false
}

// extractDiff pulls a renderable diff out of a tool result's display payload.
func extractDiff(m *store.Message) *diffView {
	if !m.Display() {
		return nil
	}
	var d struct {
		Path    string `json:"path"`
		Unified string `json:"unified"`
		Added   int    `json:"added"`
		Removed int    `json:"removed"`
	}
	if json.Unmarshal(m.ToolDisplay, &d) != nil || strings.TrimSpace(d.Unified) == "" {
		return nil
	}

	view := &diffView{Path: d.Path, Added: d.Added, Removed: d.Removed}
	for _, line := range strings.Split(strings.TrimRight(d.Unified, "\n"), "\n") {
		kind := "ctx"
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "@@"):
			kind = "meta"
		case strings.HasPrefix(line, "+"):
			kind = "add"
		case strings.HasPrefix(line, "-"):
			kind = "del"
		}
		view.Lines = append(view.Lines, diffLine{Kind: kind, Text: line})
	}
	return view
}

func extractReview(m *store.Message) *reviewView {
	if m.MessageKind() != store.KindReview || !m.Display() {
		return nil
	}
	var display struct {
		Findings string `json:"findings"`
		Clean    bool   `json:"clean"`
	}
	if json.Unmarshal(m.ToolDisplay, &display) != nil {
		return nil
	}
	if display.Findings == "" {
		return nil
	}
	// Review findings are prose from the reviewer model, which writes them as a
	// markdown list with file references in backticks.
	return &reviewView{Clean: display.Clean, Findings: renderMarkdown(display.Findings)}
}

// clip keeps a compact one-line tool summary from consuming the transcript.
func clip(text string, max int) string {
	text = strings.TrimSpace(text)
	if max <= 0 || len(text) <= max {
		return text
	}
	return text[:max] + "…"
}
