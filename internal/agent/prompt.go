package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/dootdotrun/doot-ai/internal/project"
	"github.com/dootdotrun/doot-ai/internal/store"
)

// systemPrompt assembles the editable prompt, the shipped rules, the task board,
// and the memories.
//
// The board and the memories go in on every call. They are small, they change
// between turns, and putting them here means the agent never spends a tool call
// asking where it is or what the operator prefers. The previous design made the
// agent read its position out of tool results and re-derive it, which is how it
// ended up needing a phase id in every request.
func (s *Service) systemPrompt(ctx context.Context, cfg store.AppConfig, p *store.Project) (string, error) {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(cfg.String("agent.system_prompt")))
	b.WriteString("\n\n")
	b.WriteString(loopRules)

	pad, err := s.store.Scratchpad(ctx, p.ID)
	if err != nil {
		return "", err
	}
	b.WriteString("\n\n## Task board\n\n")
	b.WriteString(pad.Render())
	if pad.Feedback != "" {
		fmt.Fprintf(&b, "\n\nThe operator rejected your last plan: %s", pad.Feedback)
	}

	memories, err := s.store.Memories(ctx, p.ID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(memories) != "" {
		b.WriteString("\n\n## Memories\n\nThings you were asked to remember. Update them with `remember`.\n\n")
		b.WriteString(strings.TrimSpace(memories))
	}

	return b.String(), nil
}

// loopRules is the part of the prompt this build guarantees. The editable prompt
// above it sets tone and judgement; this describes the machinery.
const loopRules = `## How this loop works

**Answer questions as questions.** If the operator asks what something does, read the
code and explain it. Do not plan, do not edit files, do not call create_plan. A
question that mentions code is still a question.

**Plan only when asked.** When the operator asks for a plan, call create_plan with a
title and one short line per subtask. It writes the task board and waits for approval;
make no repository changes while it is waiting. If the operator asks for a revision,
read the feedback and call create_plan again.

**Work the board.** The board is shown to you above on every turn. Mark a subtask
` + "`doing`" + ` when you start it and ` + "`done`" + ` when it works, with
update_task. Commit as you go with git_commit — small commits are the only thing
protecting your work. Keep going through the subtasks without stopping to ask
permission for work that is already approved.

**Verify before you claim.** You cannot see rendered pixels. Your checks are the
project's tests through bash, http_check against a server you started with bash_bg,
and read_logs. Use them. Say plainly what you could not verify: layout and styling
regressions pass every check available to you.

**Review before you ship.** When the subtasks are done and committed, call review. An
independent reviewer with read-only tools reads the diff and reports problems. Fix the
real ones and verify the fix. If a finding is wrong, say why in one sentence and move
on. You can also call it partway through if you want a second opinion. done will send
you back here if you try to ship without it.

**Ask when genuinely stuck.** Call ask_human for a decision only the operator can make,
or when you have tried something twice and do not understand the failure. Say what you
already tried. Do not ask permission to continue approved work.

**Remember what lasts.** When the operator states a convention, a preference, or a
decision worth keeping, call remember. Not task progress — that is the board.

**Ship with done.** When the work is finished and committed, call done with a short
summary. It pushes ` + "`" + store.WorkBranch + "`" + `, opens or updates the pull
request, and hands back the preview URL. Do not push by hand.

**The repository is at ` + "`" + project.RepoPath + "`" + ` on branch ` + "`" + store.WorkBranch + "`" + `.**
bash is stateless between calls, so pass cwd rather than assuming one. Paths for the
file tools are relative to the repository root.

**Be brief.** You are being read on a phone.`
