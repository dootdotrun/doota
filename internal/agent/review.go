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
//
// This counts model calls, not tool calls, and it used to be 5. A reviewer that is
// explicitly invited to read files and search — as the prompt below does — spends
// one turn orienting and one per read, so five meant it was cut off after three or
// four files. "Ran out of turns without reaching a conclusion" was therefore the
// ordinary outcome of a review rather than an exceptional one, and because the
// pre-ship check in ship() only asks whether a review was attempted, the work
// shipped anyway with nobody having looked at it.
const reviewerMaxTurns = 24

// reviewerWarnTurns is when the reviewer starts being told to wrap up.
//
// It cannot manage a budget it is not shown. The old loop counted privately, so the
// reviewer had no way to distinguish its first turn from its last and no reason to
// prioritise.
const reviewerWarnTurns = 3

const reviewerSystem = `You are an independent code reviewer with read-only tools. You did not write this code.

Review the diff for problems that matter: incorrect logic, unhandled errors that will
be hit, security mistakes, and claims in the summary the diff does not support. You may
read files, search, and check logs to understand context.

Report only concrete, actionable findings. For each one name the file, and say what is
wrong and why it matters. Do not report style preferences, do not suggest refactors, and
do not restate what the code does.

If the change is sound, say exactly: CLEAN

You have a limited number of turns and you will be told when they are running low.
Spend them on the parts of the diff where being wrong would matter most, and always
finish with a verdict — a review that never concludes is worth nothing to the agent
waiting on it. If you ran out of room before checking something, name it.

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
		remaining := reviewerMaxTurns - turn

		// On the final turn the tools are withheld, which is what actually guarantees
		// a verdict. Warning alone was not enough: offered a tool on its last turn, the
		// reviewer would use it, and the loop then exited on a request nobody was going
		// to answer. History always ends here on either the opening brief or a set of
		// tool results, so appending a user turn is valid at this point.
		specs := toolSpecs(s.reviewer)
		switch {
		case remaining == 1:
			specs = nil
			history = append(history, model.Message{Role: model.RoleUser,
				Content: "Last turn, and your tools are withdrawn. Give your findings now, or CLEAN if the " +
					"change is sound. Name anything you did not get to check."})
		case remaining <= reviewerWarnTurns:
			history = append(history, model.Message{Role: model.RoleUser,
				Content: fmt.Sprintf("%d turns left. Finish what you are checking and reach a verdict.", remaining)})
		}

		callCtx, cancel := context.WithCancel(ctx)
		resp, callErr := s.streamWithRetry(callCtx, model.Request{
			APIKey: cfg.Secret(store.KeyModelAPIKey), BaseURL: cfg.Text(store.KeyModelBaseURL),
			Model: cfg.Text(store.KeyModelName), System: reviewerSystem, Messages: history,
			Tools: specs, MaxTokens: cfg.Int("model.max_output_tokens"),
		}, model.Handler{})
		cancel()

		if callErr != nil || resp == nil {
			if callErr == nil {
				callErr = fmt.Errorf("the reviewer returned no response")
			}
			return tools.Result{Content: "The review did not complete: " + callErr.Error() +
				"\n\nDecide whether to retry it or proceed and say which.", IsError: true}
		}
		if cause := resp.SilentCause(); cause != model.NotSilent {
			return tools.Result{Content: "The review did not complete: " +
				silentModelError(cause, resp, cfg.Int("model.max_output_tokens")).Error() +
				"\n\nDecide whether to retry it or proceed and say which.", IsError: true}
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
	// Unreachable in normal operation: the final turn is made without tools, so it
	// cannot end in another tool call. Kept as a real answer rather than a panic in
	// case a future change to the loop above reintroduces the gap.
	return tools.Result{Content: "The reviewer stopped without reaching a conclusion. " +
		"Treat the review as inconclusive: proceed only if you are confident, and say so.", IsError: true}
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
