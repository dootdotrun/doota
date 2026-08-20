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

// reviewerMaxTurns bounds the reviewer's read-only exploration.
const reviewerMaxTurns = 5

const reviewerSystem = `You are an independent code reviewer with read-only tools. You did not write this code.

Review the diff for problems that matter: incorrect logic, unhandled errors that will
be hit, security mistakes, and claims in the summary the diff does not support. You may
read files, search, and check logs to understand context.

Report only concrete, actionable findings. For each one name the file, and say what is
wrong and why it matters. Do not report style preferences, do not suggest refactors, and
do not restate what the code does.

If the change is sound, say exactly: CLEAN

Be brief. You are being read by another agent that has to act on this.`

// reviewDisplay is the UI payload for a review result.
type reviewDisplay struct {
	Findings string `json:"findings"`
	Clean    bool   `json:"clean"`
}

// runReview shows a fresh model the diff and returns its findings as prose.
//
// The reviewer's tool history is deliberately in-memory: the result is durable as a
// transcript message, but the reviewer never inherits the primary agent's
// assumptions or conversation.
//
// It returns findings, not a verdict the loop enforces. The previous design made the
// reviewer's output a durable row with a constrained verdict enum and a matching
// commit, then required the agent to quote that row's id and write a justification
// string before Postgres would let it mark a phase complete. Six failure modes of
// that contract had their own error messages. The agent is told to fix real findings
// and say so plainly when a finding is wrong; if it lies, the operator is reading the
// same review.
func (s *Service) runReview(ctx context.Context, p *store.Project, cfg store.AppConfig, sb sandbox.Sandbox,
	runID string, messageID int64, pad store.Scratchpad, request tools.ReviewRequest) tools.Result {
	if s.reviewer == nil {
		return tools.Result{Content: "The reviewer is not configured.", IsError: true}
	}

	env := s.env(p, sb, cfg, runID, messageID, pad.BaseCommit)
	diffArgs, _ := json.Marshal(map[string]any{"from": pad.BaseCommit})
	diff, err := s.reviewer.Execute(ctx, "git_diff", diffArgs, env)
	if err != nil {
		return tools.Result{Content: "The reviewer could not collect the diff: " + err.Error(), IsError: true}
	}
	if diff.IsError {
		return tools.Result{Content: "The reviewer could not collect the diff: " + diff.Content, IsError: true}
	}
	if strings.TrimSpace(diff.Content) == "" {
		return tools.Result{Content: "There is nothing to review: the diff is empty.", IsError: true}
	}

	var b strings.Builder
	if pad.Title != "" {
		fmt.Fprintf(&b, "Intended work: %s\n", pad.Title)
		for _, t := range pad.Tasks {
			fmt.Fprintf(&b, "  %d. [%s] %s\n", t.N, t.Status, t.Summary)
		}
		b.WriteByte('\n')
	}
	if request.Focus != "" {
		fmt.Fprintf(&b, "The author asked you to look at: %s\n\n", request.Focus)
	}
	fmt.Fprintf(&b, "Diff under review:\n%s", diff.Content)

	history := []model.Message{{Role: model.RoleUser, Content: b.String()}}
	for turn := 0; turn < reviewerMaxTurns; turn++ {
		callCtx, cancel := context.WithTimeout(ctx, modelTimeout)
		resp, callErr := s.streamWithRetry(callCtx, model.Request{
			APIKey: cfg.Secret(store.KeyModelAPIKey), BaseURL: cfg.Text(store.KeyModelBaseURL),
			Model: cfg.Text(store.KeyModelName), System: reviewerSystem, Messages: history,
			Tools: toolSpecs(s.reviewer), MaxTokens: cfg.Int("model.max_output_tokens"),
		}, model.Handler{})
		cancel()

		if callErr != nil || resp == nil {
			if callErr == nil {
				callErr = fmt.Errorf("the reviewer returned no response")
			}
			return tools.Result{Content: "The review did not complete: " + callErr.Error() +
				"\n\nDecide whether to retry it or proceed and say which.", IsError: true}
		}
		if resp.Starved() {
			return tools.Result{Content: "The reviewer used its whole output budget without answering. " +
				"Raise Max output tokens in Settings, or proceed and say you did.", IsError: true}
		}

		history = append(history, model.Message{Role: model.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})

		if len(resp.ToolCalls) == 0 {
			return reviewResult(resp.Content)
		}
		for _, call := range resp.ToolCalls {
			res, toolErr := s.reviewer.Execute(ctx, call.Name, call.Args, env)
			if toolErr != nil {
				return tools.Result{Content: "The reviewer's tools failed: " + toolErr.Error() +
					"\n\nDecide whether to retry the review or proceed and say which.", IsError: true}
			}
			history = append(history, model.Message{Role: model.RoleTool, Content: res.Content, ToolCallID: call.ID})
		}
	}
	return tools.Result{Content: "The reviewer ran out of turns without reaching a conclusion. " +
		"Proceed if you are confident, and say that the review was inconclusive.", IsError: true}
}

// reviewResult turns the reviewer's prose into a tool result.
//
// CLEAN is matched rather than parsed out of JSON. The previous version required
// strict JSON with a verdict enum and a findings array, then had six separate
// rejection paths for a malformed reply — an invalid severity, a clean verdict with
// findings, findings without structure, a zero line number. All of them threw the
// review away. Prose is what the next model reads anyway.
func reviewResult(raw string) tools.Result {
	findings := strings.TrimSpace(raw)
	if findings == "" {
		return tools.Result{Content: "The reviewer replied with nothing. Treat the review as inconclusive.", IsError: true}
	}
	if clean := strings.EqualFold(strings.Trim(findings, ".!"), "CLEAN"); clean {
		return tools.Result{
			Content: "Review: clean. No actionable findings.",
			Display: reviewDisplay{Clean: true, Findings: "No actionable findings."},
		}
	}
	return tools.Result{
		Content: "Review findings:\n\n" + findings +
			"\n\nFix what is real. If a finding is wrong, say so in one sentence and move on.",
		Display: reviewDisplay{Findings: findings},
	}
}
