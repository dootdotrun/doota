package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/store"
)

// askHumanTool parks the durable run until the user replies. The reply itself is
// still a normal user message, preserving the model's ordinary conversation
// protocol instead of inventing an out-of-band answer channel.
type askHumanTool struct{}

type askHumanArgs struct {
	Question string `json:"question"`
	Context  string `json:"context"`
}

func (askHumanTool) Name() string { return "ask_human" }

func (askHumanTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "ask_human",
		Description: "Ask the user a focused question when you cannot safely continue. The run pauses until they reply in chat.",
		Params: object(map[string]Param{
			"question": {Type: "string", Description: "The specific decision or information needed."},
			"context":  {Type: "string", Description: "Brief context explaining why this answer is needed."},
		}, "question"),
	}
}

func (askHumanTool) Execute(ctx context.Context, in json.RawMessage, env *Env) (Result, error) {
	var args askHumanArgs
	if err := decode(in, &args); err != nil {
		return fail("%s", err), nil
	}
	if env == nil || env.Store == nil || env.RunID == "" {
		return Result{}, store.ErrNotFound
	}
	args.Question = strings.TrimSpace(args.Question)
	args.Context = strings.TrimSpace(args.Context)
	if args.Question == "" {
		return fail("question is required."), nil
	}
	payload := map[string]string{"question": args.Question, "context": args.Context}
	// The runner persists this result and changes the run state in one transaction.
	// A tool cannot safely make that transition itself because its matching result
	// message is owned by the runner's protocol boundary.
	content := args.Question
	if args.Context != "" {
		content += "\n\n" + args.Context
	}
	return Result{Content: content, Display: payload}, nil
}
