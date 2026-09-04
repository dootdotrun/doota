package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/model"
	"github.com/dootdotrun/doot-ai/internal/sandbox"
	"github.com/dootdotrun/doot-ai/internal/store"
	"github.com/dootdotrun/doot-ai/internal/tools"
)

// The UI subagent, which unlike everything else here can see.
//
// It exists because the primary agent cannot, and says so in its own prompt: it has
// tests, http_check and logs, and every one of those passes while a button sits
// fifteen pixels off the edge of a phone screen. That gap is the whole category of
// "fix this button" work that was being handed back to the operator to notice.
//
// Two modes on one subagent. Before building, it answers "what should this look
// like" and produces a brief the primary agent can implement against. After
// building — and after the semantic reviewer, so the code is already known to be
// sound — it opens the running server in a real browser and answers "does it".
//
// It shares the shape of runReview deliberately: own history, own read-only registry,
// bounded turns, tools withheld on the last one so a verdict always arrives.

const (
	uiReviewerMaxTurns  = 20
	uiReviewerWarnTurns = 3

	// Viewports. A narrow phone and an ordinary laptop, which is where layout
	// actually breaks — the operator reads this tool on a phone, so that one is not
	// optional.
	phoneWidth    = 390
	phoneHeight   = 844
	desktopWidth  = 1440
	desktopHeight = 900

	// maxShots bounds one review. Each image is a real fraction of the request, and
	// past this the reviewer is skimming rather than looking.
	maxShots = 8
)

const uiDesignSystem = `You are a product designer briefing an engineer who cannot see. Be concrete.

You are given what they intend to build. Return a design brief they can implement
without making a single visual judgement themselves:

- Layout and hierarchy: what is most important on this screen, what is secondary,
  what should be hidden until asked for.
- Spacing and size in real units. Not "add some padding" — say 12px.
- Every state that needs a design: empty, loading, error, disabled, too-long text.
- Responsive behaviour: what changes between a 390px phone and a 1440px desktop,
  and what must not.
- Touch targets, contrast, and focus states, in numbers.

Two rules on scope. Work with the design language the project already has rather
than importing your taste — if you have not been told what that is, say what you
assumed. And do not design what was not asked for; a brief for a button is a brief
for a button.

Be brief and specific. You are being read by an agent that has to build this.`

const uiVerifySystem = `You are a designer reviewing a built interface. You can see it; the engineer could not.

You are given the intent, and screenshots of the running page at phone and desktop
widths. You may take more screenshots at other sizes or other paths, and read the
source to understand what produced something.

Report only what is actually wrong on screen, and name the file that causes it where
you can:

- Overflow, clipping, and truncation. Text running out of its container, horizontal
  scroll on a phone, controls off the edge.
- Alignment, spacing, and hierarchy that fights the intent.
- Illegible or low-contrast text, tap targets too small for a thumb.
- States that render badly: an empty list, a long string, an error.
- Anything present that the intent did not ask for, and anything asked for that is
  not there.

Do not report code style, do not suggest refactors, and do not restate what the
screen contains. If something looks deliberate and works, leave it alone.

If the interface is sound, say exactly: CLEAN

Be brief and specific — file and fix, one line each. You are being read by an agent
that has to act on this.`

// uiReviewDisplay is the UI payload for a ui_review result.
type uiReviewDisplay struct {
	Mode     string   `json:"mode"`
	Findings string   `json:"findings"`
	Clean    bool     `json:"clean"`
	Shots    []string `json:"shots,omitempty"`
}

// runUIReview shows a fresh model the intent and, in verify mode, the rendered page.
func (s *Service) runUIReview(ctx context.Context, p *store.Project, cfg store.AppConfig, sb sandbox.Sandbox,
	runID string, messageID int64, pad store.Scratchpad, request tools.UIReviewRequest) tools.Result {
	if s.uiReviewer == nil {
		return tools.Result{Content: "The UI reviewer is not configured.", IsError: true}
	}

	env := s.env(p, sb, cfg, runID, messageID, pad.BaseCommit)
	system := uiVerifySystem
	if request.Mode == tools.UIModeDesign {
		system = uiDesignSystem
	}

	var brief strings.Builder
	fmt.Fprintf(&brief, "Intent: %s\n", request.Intent)
	if pad.Title != "" {
		fmt.Fprintf(&brief, "\nThis is part of: %s\n", pad.Title)
	}
	if request.Focus != "" {
		fmt.Fprintf(&brief, "\nLook especially at: %s\n", request.Focus)
	}

	opening := model.Message{Role: model.RoleUser}

	if request.Mode == tools.UIModeVerify {
		shots, notes, err := s.capture(ctx, env, request)
		if err != nil {
			return tools.Result{Content: "The UI review could not photograph the page: " + err.Error() +
				"\n\nFix that and call ui_review again, or proceed and say it was not visually checked.",
				IsError: true}
		}
		if len(shots) == 0 {
			return tools.Result{Content: "The UI review took no usable screenshots. " + notes +
				"\n\nCheck the server is running and the URL is right, then call ui_review again.",
				IsError: true}
		}
		fmt.Fprintf(&brief, "\nScreenshots follow, in this order:\n%s", notes)
		for _, shot := range shots {
			opening.Images = append(opening.Images, model.Image{MIME: "image/png", Data: shot.data})
		}
	} else {
		brief.WriteString("\nNothing is built yet. Produce the brief.\n")
	}

	opening.Content = brief.String()
	history := []model.Message{opening}

	for turn := 0; turn < uiReviewerMaxTurns; turn++ {
		remaining := uiReviewerMaxTurns - turn

		// Same contract as the semantic reviewer: warn as the budget runs down, and
		// withhold the tools entirely on the last turn so it cannot end on a request
		// nobody is going to answer.
		specs := toolSpecs(s.uiReviewer)
		switch {
		case remaining == 1:
			specs = nil
			history = append(history, model.Message{Role: model.RoleUser,
				Content: "Last turn, and your tools are withdrawn. Give your findings now, or CLEAN if it " +
					"is sound. Name anything you did not get to check."})
		case remaining <= uiReviewerWarnTurns:
			history = append(history, model.Message{Role: model.RoleUser,
				Content: fmt.Sprintf("%d turns left. Reach a verdict.", remaining)})
		}

		callCtx, cancel := context.WithCancel(ctx)
		resp, callErr := s.streamWithRetry(callCtx, model.Request{
			APIKey: cfg.Secret(store.KeyModelAPIKey), BaseURL: cfg.Text(store.KeyModelBaseURL),
			Model: cfg.Text(store.KeyModelName), System: system, Messages: history,
			Tools: specs, MaxTokens: cfg.Int("model.max_output_tokens"),
			ReasoningEffort: cfg.Text(store.KeyReasoningEffort),
		}, model.Handler{})
		cancel()

		if callErr != nil || resp == nil {
			if callErr == nil {
				callErr = fmt.Errorf("the UI reviewer returned no response")
			}
			return tools.Result{Content: "The UI review did not complete: " + callErr.Error() +
				"\n\nDecide whether to retry it or proceed and say which.", IsError: true}
		}
		if cause := resp.SilentCause(); cause != model.NotSilent {
			return tools.Result{Content: "The UI review did not complete: " +
				silentModelError(cause, resp, cfg.Int("model.max_output_tokens")).Error() +
				"\n\nDecide whether to retry it or proceed and say which.", IsError: true}
		}

		history = append(history, model.Message{Role: model.RoleAssistant, Content: resp.Content,
			ToolCalls: resp.ToolCalls, Reasoning: resp.Reasoning})

		if len(resp.ToolCalls) == 0 {
			return uiReviewResult(request.Mode, resp.Content)
		}
		for _, call := range resp.ToolCalls {
			res, toolErr := s.uiReviewer.Execute(ctx, call.Name, call.Args, env)
			if toolErr != nil {
				return tools.Result{Content: "The UI reviewer's tools failed: " + toolErr.Error() +
					"\n\nDecide whether to retry the review or proceed and say which.", IsError: true}
			}

			result := model.Message{Role: model.RoleTool, Content: res.Content, ToolCallID: call.ID}
			history = append(history, result)

			// A screenshot the reviewer asked for itself has to actually reach it, and a
			// tool result on this transport is text. So the file it just wrote is
			// attached as a following user turn.
			if !res.IsError && call.Name == "screenshot" {
				if shot, ok := res.Display.(tools.ScreenshotResult); ok {
					if data, readErr := tools.ReadScreenshot(ctx, sb, shot.Path); readErr == nil {
						history = append(history, model.Message{Role: model.RoleUser,
							Content: fmt.Sprintf("%s — %s at %dx%d:", shot.Label, shot.URL, shot.Width, shot.Height),
							Images:  []model.Image{{MIME: "image/png", Data: data}}})
					} else {
						history = append(history, model.Message{Role: model.RoleUser,
							Content: "That screenshot could not be read back: " + readErr.Error()})
					}
				}
			}
		}
	}

	// Unreachable while the final turn is made without tools.
	return tools.Result{Content: "The UI reviewer stopped without reaching a conclusion. " +
		"Treat it as unchecked: proceed only if you are confident, and say so.", IsError: true}
}

// shot is one capture and its bytes.
type shot struct {
	label string
	data  []byte
}

// capture photographs every requested path at every requested width.
//
// Failures are collected rather than fatal: one bad path out of four should still
// produce a review of the other three, and the reviewer is told which ones did not
// work so it does not report an empty page as a design defect.
func (s *Service) capture(ctx context.Context, env *tools.Env, request tools.UIReviewRequest) ([]shot, string, error) {
	type viewport struct {
		name          string
		width, height int
	}
	var viewports []viewport
	for _, d := range request.Devices {
		switch d {
		case "phone":
			viewports = append(viewports, viewport{"phone", phoneWidth, phoneHeight})
		case "desktop":
			viewports = append(viewports, viewport{"desktop", desktopWidth, desktopHeight})
		}
	}
	if len(viewports) == 0 {
		viewports = []viewport{{"phone", phoneWidth, phoneHeight}, {"desktop", desktopWidth, desktopHeight}}
	}

	var shots []shot
	var notes strings.Builder
	for _, p := range request.Paths {
		for _, v := range viewports {
			if len(shots) >= maxShots {
				fmt.Fprintf(&notes, "- stopped after %d screenshots\n", maxShots)
				return shots, notes.String(), nil
			}
			label := fmt.Sprintf("%s-%s", strings.Trim(strings.ReplaceAll(p, "/", "-"), "-"), v.name)
			if label == "-"+v.name || label == v.name {
				label = "root-" + v.name
			}
			args, _ := json.Marshal(map[string]any{
				"url": request.URL + p, "label": label, "width": v.width, "height": v.height,
			})
			res, err := s.uiReviewer.Execute(ctx, "screenshot", args, env)
			if err != nil {
				return nil, notes.String(), err
			}
			if res.IsError {
				fmt.Fprintf(&notes, "- %s %s: FAILED — %s\n", p, v.name, firstLine(res.Content))
				continue
			}
			meta, ok := res.Display.(tools.ScreenshotResult)
			if !ok {
				fmt.Fprintf(&notes, "- %s %s: FAILED — no image metadata\n", p, v.name)
				continue
			}
			data, readErr := tools.ReadScreenshot(ctx, env.Sandbox, meta.Path)
			if readErr != nil {
				fmt.Fprintf(&notes, "- %s %s: FAILED — %s\n", p, v.name, readErr.Error())
				continue
			}
			fmt.Fprintf(&notes, "- %s at %dx%d (%s)\n", p, meta.Width, meta.Height, v.name)
			shots = append(shots, shot{label: meta.Label, data: data})
		}
	}
	return shots, notes.String(), nil
}

// uiReviewResult turns the reviewer's prose into a tool result.
func uiReviewResult(mode, raw string) tools.Result {
	findings := strings.TrimSpace(raw)
	if findings == "" {
		return tools.Result{Content: "The UI reviewer replied with nothing. Treat it as unchecked.", IsError: true}
	}

	if mode == tools.UIModeDesign {
		return tools.Result{
			Content: "Design brief:\n\n" + findings +
				"\n\nBuild to this. If you disagree with something, say which and why in one sentence.",
			Display: uiReviewDisplay{Mode: mode, Findings: findings},
		}
	}

	if strings.EqualFold(strings.Trim(findings, ".!"), "CLEAN") {
		return tools.Result{
			Content: "UI review: clean. Nothing visually wrong.",
			Display: uiReviewDisplay{Mode: mode, Clean: true, Findings: "Nothing visually wrong."},
		}
	}
	return tools.Result{
		Content: "UI review findings:\n\n" + findings +
			"\n\nFix what is real. If a finding is wrong, say so in one sentence and move on.",
		Display: uiReviewDisplay{Mode: mode, Findings: findings},
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
