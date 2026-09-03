package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/store"
)

// The control tools validate a request and hand it to the runner, which owns the
// model client and the durable transitions. They are deliberately thin.

// PlanRequest is a spec plus an ordered list of subtasks.
//
// It carries a spec because the operator is not a programmer. A list of subtask
// titles is not reviewable by someone who cannot read the resulting diff, so the
// approval gate was asking them to approve something they had no way to assess. A
// restated problem, an approach in plain language, and a named verification for each
// claim are reviewable without being able to read code.
type PlanRequest struct {
	Title        string   `json:"title"`
	Problem      string   `json:"problem"`
	Approach     string   `json:"approach"`
	Verification []string `json:"verification"`
	EdgeCases    []string `json:"edge_cases"`
	Risks        string   `json:"risks"`
	Questions    []string `json:"questions"`
	Tasks        []string `json:"tasks"`
}

type createPlanTool struct{}

func (createPlanTool) Name() string { return "create_plan" }
func (createPlanTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "create_plan",
		Description: "Write a spec and plan for work that changes the repository, and wait for the operator to " +
			"approve it. Changes no files. This is the normal way to start any change — orient yourself first, " +
			"then plan. The operator is not a programmer, so write problem, approach, and risks in plain " +
			"language they can actually judge.",
		Params: object(map[string]Param{
			"title":    {Type: "string", Description: "What this achieves, in a few words."},
			"problem":  {Type: "string", Description: "The goal restated in your own words, so a misunderstanding surfaces now rather than after the work."},
			"approach": {Type: "string", Description: "How you will do it, in plain language a non-programmer can follow."},
			"verification": {Type: "array", Items: &Param{Type: "string"},
				Description: "How you will check each claim: named commands, named URLs, specific observations. Not \"test it\"."},
			"edge_cases": {Type: "array", Items: &Param{Type: "string"},
				Description: "What could break that the request did not mention. Empty inputs, failure paths, existing callers, concurrent use. This is the thinking the operator cannot do for themselves, so do it properly."},
			"risks":     {Type: "string", Description: "What might go wrong, and anything you are unsure about."},
			"questions": {Type: "array", Items: &Param{Type: "string"}, Description: "Anything genuinely ambiguous. The operator can still approve; say which assumption you will proceed on."},
			"tasks":     {Type: "array", Items: &Param{Type: "string"}, Description: "Ordered subtasks, one short line each."},
		}, "title", "problem", "approach", "verification", "tasks"),
	}
}
func (createPlanTool) Execute(_ context.Context, in json.RawMessage, _ *Env) (Result, error) {
	var args PlanRequest
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	args.Title = strings.TrimSpace(args.Title)
	if args.Title == "" {
		return fail("title is required."), nil
	}
	args.Problem = strings.TrimSpace(args.Problem)
	if args.Problem == "" {
		return fail("problem is required: restate what the operator wants in your own words."), nil
	}
	args.Approach = strings.TrimSpace(args.Approach)
	if args.Approach == "" {
		return fail("approach is required: say how you will do this, in plain language."), nil
	}
	args.Risks = strings.TrimSpace(args.Risks)

	args.Verification = trimAll(args.Verification)
	if len(args.Verification) == 0 {
		return fail("verification is required: name how you will check this actually works. " +
			"If something genuinely cannot be verified — rendered layout, for instance — say that as the entry."), nil
	}
	args.EdgeCases = trimAll(args.EdgeCases)
	args.Questions = trimAll(args.Questions)

	args.Tasks = trimAll(args.Tasks)
	if len(args.Tasks) == 0 {
		return fail("a plan needs at least one subtask."), nil
	}

	return Result{
		Content: fmt.Sprintf("Proposed %q: %d subtasks, %d verification steps, %d edge cases. "+
			"Waiting for the operator to approve it.",
			args.Title, len(args.Tasks), len(args.Verification), len(args.EdgeCases)),
		Display: args,
	}, nil
}

// trimAll drops blank entries, which a model padding an array to look thorough will
// produce.
func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// TaskUpdate moves one line on the board.
type TaskUpdate struct {
	Task   int    `json:"task"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

type updateTaskTool struct{}

func (updateTaskTool) Name() string { return "update_task" }
func (updateTaskTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "update_task",
		Description: "Update a subtask on the task board so the operator can see where you are. The board is " +
			"shown to you on every turn, so use the numbers from it.",
		Params: object(map[string]Param{
			"task":   {Type: "integer", Description: "Subtask number from the board."},
			"status": {Type: "string", Enum: []string{store.TaskPending, store.TaskDoing, store.TaskDone, store.TaskBlocked}, Description: "New status."},
			"note":   {Type: "string", Description: "Optional one-line note: what you did, or what is blocking."},
		}, "task", "status"),
	}
}
func (updateTaskTool) Execute(_ context.Context, in json.RawMessage, _ *Env) (Result, error) {
	var args TaskUpdate
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	if args.Task <= 0 {
		return fail("task must be a subtask number from the board."), nil
	}
	args.Status = strings.TrimSpace(args.Status)
	switch args.Status {
	case store.TaskPending, store.TaskDoing, store.TaskDone, store.TaskBlocked:
	default:
		return fail("status must be pending, doing, done, or blocked."), nil
	}
	args.Note = strings.TrimSpace(args.Note)
	return Result{Content: fmt.Sprintf("Marking subtask %d as %s.", args.Task, args.Status), Display: args}, nil
}

// MemoriesUpdate replaces the durable memories text.
type MemoriesUpdate struct {
	Memories string `json:"memories"`
}

type rememberTool struct{}

func (rememberTool) Name() string { return "remember" }
func (rememberTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "remember",
		Description: "Replace your durable memories: the operator's conventions, preferences, and decisions worth " +
			"surviving a cleared conversation. You are shown the current memories every turn; return the full " +
			"updated text, editing or pruning as needed. Keep it short. Do not record task progress here — that is " +
			"what the task board is for.",
		Params: object(map[string]Param{
			"memories": {Type: "string", Description: "The complete new memories text."},
		}, "memories"),
	}
}
func (rememberTool) Execute(_ context.Context, in json.RawMessage, _ *Env) (Result, error) {
	var args MemoriesUpdate
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	args.Memories = strings.TrimSpace(args.Memories)
	if len(args.Memories) > maxNotesLen {
		return fail("memories must stay under %d characters; prune the least useful lines.", maxNotesLen), nil
	}
	return Result{Content: "Memories updated.", Display: args}, nil
}

// maxNotesLen caps both durable notes. They go into every system prompt, so the cost
// of an unbounded one is paid on every call for the rest of the project.
const maxNotesLen = 8000

// OrientationUpdate replaces what the agent knows about the repository.
type OrientationUpdate struct {
	Orientation string `json:"orientation"`
}

type orientTool struct{}

func (orientTool) Name() string { return "record_orientation" }
func (orientTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "record_orientation",
		Description: "Record what you learned about this repository so you do not have to work it out again " +
			"next conversation: how to build it, how to run its tests and linters, how to start it locally, " +
			"where things live, and the conventions it actually follows. You are shown the current notes every " +
			"turn; return the full updated text, correcting anything that turned out to be wrong. " +
			"Facts about the code, not the operator's preferences — those are `remember`.",
		Params: object(map[string]Param{
			"orientation": {Type: "string", Description: "The complete new orientation notes, as markdown."},
		}, "orientation"),
	}
}
func (orientTool) Execute(_ context.Context, in json.RawMessage, _ *Env) (Result, error) {
	var args OrientationUpdate
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	args.Orientation = strings.TrimSpace(args.Orientation)
	if args.Orientation == "" {
		return fail("orientation is required. To clear the notes, say so in chat instead."), nil
	}
	if len(args.Orientation) > maxNotesLen {
		return fail("orientation must stay under %d characters; keep the commands and drop the prose.", maxNotesLen), nil
	}
	return Result{Content: "Orientation notes updated.", Display: args}, nil
}
