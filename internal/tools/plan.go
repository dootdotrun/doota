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

// PlanRequest is a title and one line per subtask. That is the whole shape.
type PlanRequest struct {
	Title string   `json:"title"`
	Tasks []string `json:"tasks"`
}

type createPlanTool struct{}

func (createPlanTool) Name() string { return "create_plan" }
func (createPlanTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "create_plan",
		Description: "Propose a plan as an ordered list of subtasks, one line each. Writes the task board and " +
			"waits for the operator to approve it on the Plan screen. Changes no files. Only call this when the " +
			"operator has asked for a plan.",
		Params: object(map[string]Param{
			"title": {Type: "string", Description: "What this plan achieves, in a few words."},
			"tasks": {Type: "array", Description: "Ordered subtasks, one short line each.", Items: &Param{Type: "string"}},
		}, "title", "tasks"),
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
	cleaned := make([]string, 0, len(args.Tasks))
	for _, t := range args.Tasks {
		if t = strings.TrimSpace(t); t != "" {
			cleaned = append(cleaned, t)
		}
	}
	if len(cleaned) == 0 {
		return fail("a plan needs at least one subtask."), nil
	}
	args.Tasks = cleaned
	return Result{
		Content: fmt.Sprintf("Proposed %q with %d subtasks. Waiting for approval on the Plan screen.", args.Title, len(cleaned)),
		Display: args,
	}, nil
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
	if len(args.Memories) > 8000 {
		return fail("memories must stay under 8000 characters; prune the least useful lines."), nil
	}
	return Result{Content: "Memories updated.", Display: args}, nil
}
