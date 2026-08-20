package tools

import (
	"context"
	"encoding/json"
	"strings"
)

// ReviewRequest asks for a semantic review of the work so far.
//
// No arguments identifying what to review: the reviewer is always shown the diff
// since the plan was approved, or the working tree when there is no plan. Letting
// the agent choose its own review scope invited it to review the part it felt good
// about.
type ReviewRequest struct {
	Focus string `json:"focus"`
}

type reviewTool struct{}

func (reviewTool) Name() string { return "review" }
func (reviewTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "review",
		Description: "Send your work to an independent reviewer with read-only tools. It sees the diff since the " +
			"plan was approved and reports concrete problems. Call it when the work is done, or partway through if " +
			"you want a check. Fix real findings; say so plainly if a finding is wrong.",
		Params: object(map[string]Param{
			"focus": {Type: "string", Description: "Optional: anything specific the reviewer should look at."},
		}),
	}
}
func (reviewTool) Execute(_ context.Context, in json.RawMessage, _ *Env) (Result, error) {
	var args ReviewRequest
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	args.Focus = strings.TrimSpace(args.Focus)
	return Result{Content: "Starting an independent review.", Display: args}, nil
}

// DoneRequest finishes the work: push and open a pull request.
type DoneRequest struct {
	Summary string `json:"summary"`
}

type doneTool struct{}

func (doneTool) Name() string { return "done" }
func (doneTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "done",
		Description: "Ship the work: push the doot branch, open or update a pull request, and hand back the " +
			"preview URL. Requires a clean worktree, so commit first. This ends the run.",
		Params: object(map[string]Param{
			"summary": {Type: "string", Description: "Short summary of what you did and how you verified it."},
		}, "summary"),
	}
}
func (doneTool) Execute(_ context.Context, in json.RawMessage, _ *Env) (Result, error) {
	var args DoneRequest
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	args.Summary = strings.TrimSpace(args.Summary)
	if args.Summary == "" {
		return fail("summary is required."), nil
	}
	return Result{Content: "Shipping.", Display: args}, nil
}
