# doota: diagnosis report

Written before any changes. Every claim below points at the code that causes it.
Confidence is stated where I could not prove something from the repo alone.

Verified: `go build ./...` and `go vet ./...` are both clean. Nothing here is a
compile problem. These are logic, prompt, and design issues.

---

## 1. The reviewer subagent almost always "runs out of turns"

**It is not mixed with the primary loop.** That part is fine. `runReview` builds its
own `history` slice in memory (`internal/agent/review.go`), separate system prompt,
separate read-only registry. It cannot see or consume the primary agent's context.

**The actual cause is that the budget is five model calls.**

`internal/agent/review.go:18`

```go
const reviewerMaxTurns = 5
```

That counter increments once per *model call*, not once per tool call. So a turn
spent asking for `read_file` is a turn gone. Meanwhile the reviewer's own system
prompt actively invites exploration:

> You may read files, search, and check logs to understand context.

A competent reviewer given a diff and read-only tools will orient (1 turn), then
read two or three files (3 turns), and now has one turn left for a verdict — if it
is lucky. Running out is the *normal* outcome here, not an edge case. That matches
what you are seeing.

Three things make it worse:

- **It is never told how many turns it has.** The loop counts privately. The
  reviewer has no way to know it should wrap up.
- **Tools are offered on every single turn**, including the last one
  (`Tools: toolSpecs(s.reviewer)` inside the loop). There is no final tools-free
  call that forces an answer. So the last turn gets spent on another tool request
  and the loop exits with nothing.
- **When it does run out, nothing enforces anything.** The out-of-turns result is
  `IsError`, but `ship()` gates on `store.ReviewAttempted` — which, per its own
  doc comment in `internal/store/history.go`, checks *attempted, not passed*. So a
  reviewer that never once reached a conclusion still satisfies the pre-ship
  guard. The review step is currently decorative.

Also: the reviewer shares `model.max_output_tokens` with the primary agent
(`review.go:89`), so it inherits the starvation problem in §2 as well.

**Fix direction.** Raise the budget to something a reviewer can actually work in
(20–25), tell it its remaining budget in the message, and make the final turn a
forced tools-free call so it always produces a verdict. Separately, decide whether
`done` should distinguish "reviewed and clean" from "review never completed" —
right now it cannot.

---

## 2. "Model exceeded its output token limits in reasoning" and the 400

This is three distinct problems wearing one error message. Taking them in order of
how much they explain.

### 2a. The error message is a catch-all pretending to be specific

`internal/model/model.go:171`

```go
func (r *Response) Starved() bool {
	return r.Silent() && (r.Truncated || !r.UsageReported)
}
```

`UsageReported` is only ever set when a usage chunk arrives with a non-zero token
count. So `!UsageReported` is true for **any** stream that ended with no content,
no tool calls, and no usage chunk — whatever the reason. A dropped connection, a
filtered response, an empty completion, a provider hiccup: all of them get
reported as budget exhaustion.

And the message the loop prints is very specific:

`internal/agent/agent.go:330`

```go
return false, fmt.Errorf("model exhausted its output budget on reasoning and returned nothing; raise Max output tokens from %d in Settings", ...)
```

So your instinct is right — it usually is *not* what happened. You are reading a
guess phrased as a diagnosis. This one line is responsible for most of the
confusion in this issue.

**Also worth knowing: your budget is not 132k.** The shipped default is 16,384:

`internal/store/appconfig.go:114`

```go
Key: "model.max_output_tokens", ..., Default: 16384,
```

16k, on a reasoning model that spends the budget on reasoning first, genuinely
*can* be exhausted. So some of these may be real — but you cannot tell which,
because 2a collapses every failure into the same sentence.

### 2b. The 400 Bad Request

Leading hypothesis, **high confidence**, two compounding causes in the same place.

`internal/model/model.go:453`

```go
if req.MaxTokens > 0 {
	params.MaxTokens = param.NewOpt(int64(req.MaxTokens))
}
```

The SDK's `MaxTokens` serializes as **`max_tokens`**. That is the legacy field.
Meta's own provider configuration for Muse Spark uses **`max_completion_tokens`**
alongside `reasoning_effort`, per the
[Meta Model API provider docs](https://www.promptfoo.dev/docs/providers/meta/).
Reasoning-model endpoints on OpenAI-compatible surfaces commonly reject
`max_tokens` outright, and a rejected parameter is exactly a
`400 invalid_request_error` with a generic "check the request body" body — which is
the error you pasted.

Second, and independently: **there is no upper bound on the value.**
`store.ParseValue` only enforces greater-than-zero. Muse Spark's documented output
ceiling is 131,072 tokens ([Meta Model API
docs](https://www.promptfoo.dev/docs/providers/meta/), 1,048,576-token context /
131,072 max output — the "132k approx" you remembered). So the starvation message
in 2a *instructs you to raise a number* that, past 131,072, guarantees a permanent
400. And `Resume` replays the byte-identical request, which is why you get
"The model request was retried and still failed."

That is the loop you fell into: too-low default → misleading starvation error →
you raise the number → invalid request → 400 forever.

Cheap way to confirm before we change anything: one `curl` against the endpoint
with `max_tokens` and then with `max_completion_tokens`, same body otherwise. I can
do that once you have an API key in place.

Two smaller things in the same area:

- `reasoning_effort` is never sent. Per the docs it accepts
  `minimal|low|medium|high|xhigh` and picks its own depth when omitted. For an
  agent that should be careful (§5), this is the single most direct lever we have
  and it is currently unused.
- `agent.modelTimeout` is 30 minutes but `model.streamTimeout` is 10 and applies
  the inner context, so the effective cap is 10. The 30 is dead and misleading.

### 2c. The real cost: reasoning is thrown away on every turn

This is the finding I would most want you to read.

Meta's tool-calling documentation states that the **Responses API** is the
recommended path for tool calling and multi-turn agents, because it *preserves
reasoning across tool turns* for stronger multi-step performance, threaded either
by `previous_response_id` or by stateless reasoning replay. The
[overview docs](https://ai.developer.meta.com/docs/overview/) list "reasoning that
carries across turns" as a headline Muse Spark primitive.

doota uses **Chat Completions**:

```go
stream := api.Chat.Completions.NewStreaming(ctx, params)
```

…which drops it. And the scaffolding for the alternative is half-built and dead:

- `message.reasoning` column exists (`internal/store/migrations/001_init.sql:119`)
- `Message.Reasoning` and `NewMessage.Reasoning` fields exist
- **Nothing ever writes them, and nothing ever sends them back.** Confirmed by
  grep: the only references are the struct definitions and the INSERT parameter
  lists.

The package doc in `model.go` argues at length that there is nothing to replay
because the reasoning text is returned nowhere. It also says, explicitly, that
this was *"measured against Muse Spark 1.2"*. You are running **1.3-contributor**.
The config default is still `muse-spark-1.2` (`appconfig.go`), so the whole
package's central assumption is calibrated to a different model than the one you
use, on the transport that is documented not to carry reasoning.

**Consequence.** Every turn the model re-derives its reasoning from scratch,
because the previous turn's thinking is gone. That means the output budget goes
disproportionately to reasoning on *every* call, which makes genuine starvation
far more likely — and it means multi-step tool work is being done by a model that
keeps losing its own thread. This is also the honest answer to your "1M context
model shouldn't feel exhausted this fast" instinct: it is not the context window,
it is that the reasoning chain is severed at every tool boundary.

---

## 3. The UI shakes, auto-refreshes, and scrolls

All of it is `internal/web/static/chat.js`. Five separate mechanisms, and they
compound.

**1. Full page reloads are being used as a refresh mechanism.**

```js
source.addEventListener("run.state", reload);
source.addEventListener("plan.updated", reload);
```

`reload()` calls `window.location.reload()`. Each one destroys the document and
therefore the `EventSource`, which reconnects, which fires `open`, which calls
`tail()` again. This is your "auto refreshing entire page".

**2. The streaming placeholder is shown and hidden in a fight with itself.**
`message.delta` un-hides `#streaming` and appends text (batched every 100ms by
`deltaFlush` in `agent.go`). But any `message.complete` / `tool.complete` /
`message.created` calls `tail()`, and `tail()` calls `clearStream()`, which hides
it again. Next delta shows it again. A block that grows, vanishes, and reappears
several times a second — that is the shake.

**3. The agent bar toggles `hidden`.** `setState()` adds and removes `.hidden` on
`#agent-bar`, which sits *above* the transcript. Every state change therefore
shifts every message on screen vertically. No space is reserved for it.

**4. Scroll position is fought over.** `scroll()` is called on every delta and
every append. Worse, the guard and the action disagree:

```js
function atBottom() {
  return window.innerHeight + window.scrollY >= document.body.offsetHeight - 160;
}
function scroll(force) {
  if (force || atBottom()) window.scrollTo(0, document.body.scrollHeight);
}
```

`offsetHeight` and `scrollHeight` are different numbers. The test for "is the user
at the bottom" is measured against one and the jump target is the other.

**5. Reloads queue up and then fire in a burst.** `composerBusy()` defers reloads
while the composer has text or focus, setting `reloadPending`. When you tap away,
`drain()` fires the deferred reload — so the page sits still while you type and
then lurches the moment you stop.

### On a separate frontend

**My recommendation: don't, and your instinct is correct.**

This is not a Go problem and not a monolith problem. The architecture here is
actually good — the server owns rendering, `/chat/tail` returns canonical HTML,
and there is exactly one implementation of a message bubble. That design is why
the live view can't drift from a page reload. Keep it.

Every symptom above lives in about 150 lines of client JS that chose
reload-as-refresh, plus a layout that shifts. A Svelte or React app on Cloudflare
Pages would add cross-origin auth, CORS, a second deploy target, and a *second*
renderer to keep in sync — and would fix none of the five causes, because none of
them are caused by the server. Decoupling is not necessary yet.

Fix direction: replace `reload()` with targeted fragment swaps (htmx is already
loaded and is not being used for this), reserve fixed height for the agent bar,
keep the streaming placeholder mounted and just empty it, and gate scrolling on a
single consistent measurement.

---

## 4. The UI teaches instead of getting out of the way

Taking your ADHD point as a hard requirement, not a nice-to-have.

### The delete modal

`internal/web/templates/layout.html` — two full paragraphs, roughly 50 words,
every time:

> Permanently deletes every message, run, tool call, and the task board. This is
> not an archive and there is no undo.
>
> The sandbox is **not** touched. The repo, the `doot` branch, your working tree,
> anything still running, and remembered notes all survive […]

You know this. It should be "Clear conversation?" / Cancel / Clear.

### Settings dumps everything open — and here is the exact reason

`internal/web/settings.go`, in `buildGroups`:

```go
// A group is collapsed when everything in it is a full-height textarea
g.Collapsed = true
for _, f := range g.Fields {
	if f.Kind != string(store.KindTextarea) {
		g.Collapsed = false
		break
	}
}
```

A group collapses **only if every field in it is a textarea**. So:

| Group | Collapsed? |
|---|---|
| Credentials | no — 3 secret inputs |
| Model | no — text + int |
| Agent | yes (system prompt only) |
| Sandbox | yes (setup script only) |
| Git | no — 2 text inputs |

Three of five groups can never collapse. And below the config form, *outside* the
group loop entirely, sit four more always-open sections: Sign-in, Session, and the
whole Project block (create form / status / preview / sandbox recovery). That is
the "everything dumped" feeling, and it is mechanical, not subjective.

The template already supports `.Collapsed` and `<details>`. Fields inside a closed
`<details>` still submit. So this is close to a one-line change plus wrapping the
four trailing cards.

### Help text is unconditional

`settings.html` renders `{{if .Help}}<p class="help">{{.Help}}</p>{{end}}` on
every field, and several are multi-sentence explainers — the max-output-tokens
note, the setup-script note, the repository-URL paragraph. Plus the chat screen's
empty state ("Say something / Ask about the code, or ask for a change…"), the plan
approval blurb, and the `sandboxBlockedMessage` prose. All teaching copy.

### The header icons

**These are not a CSS bug.** The CSS is correct — `app.css:375`:

```css
.iconbtn svg {
  width: 19px; height: 19px;
  fill: none; stroke: currentColor;
  stroke-width: 1.8;
  stroke-linecap: round; stroke-linejoin: round;
}
```

`fill: none` and `stroke: currentColor` are both there. The problem is the **path
data itself**. The settings gear is a circle plus six disconnected tick marks:

```
<circle cx="12" cy="12" r="3"/>
<path d="M12 3v3m0 12v3M4.2 7.5l2.6 1.5m10.4 6-2.6-1.5M4.2 16.5l2.6-1.5m10.4-6-2.6 1.5"/>
```

Those are hand-written stubs, not a gear. At 19px with a 1.8 stroke they render as
a smudge around a circle. Same class of problem on the reload and preview glyphs.
So the fix is swapping in a real icon set (inline SVG, no dependency), not
patching CSS.

*Caveat: I read this off the path data rather than a screenshot. If you want, I
can render the header standalone and show you before/after.*

### PWA identity

`static/manifest.webmanifest`:

- `"id": "/"` but `"start_url": "/app/"` and `"scope": "/app/"`. The `id` should
  normally match the scope. A mismatched `id` is a real cause of an installed app
  feeling like a *different* app — it changes how the browser identifies the
  installation.
- No splash configuration beyond `background_color: #ffffff` and
  `theme_color: #ffffff`. Both platforms therefore generate a blank white splash
  with the (weak) icon centred on it. That is the "different app" feeling.
- `icon-maskable-512.png` is RGB with no alpha channel, while the other two are
  RGBA — verified with `file`. Inconsistent, and maskable icons want deliberate
  safe-zone padding.

---

## 5. It behaves like a generic agent because the prompt tells it to

This one is mostly **not a bug**. It is the shipped design, and the design is
aimed at a different operator than you.

### It skips planning because it is instructed to, twice

`internal/agent/prompt.go`, in `loopRules`:

> **Plan only when asked.** When the operator asks for a plan, call create_plan […]

And again in the tool's own description, `internal/tools/plan.go`:

> **Only call this when the operator has asked for a plan.**

So "it avoided creating a plan and taking approval" is the prompt working exactly
as written. For a non-technical operator who depends on this for implementation,
plan-and-approve should be the *default* path, with a fast lane for genuine
questions — the inverse of what is there now.

### There is no orientation step at all

Neither `DefaultSystemPrompt` (`internal/store/prompt.go`) nor `loopRules`
contains any instruction to read the repository before acting. No README, no
package manifest, no test-command discovery, no conventions file, no build check.
Your "no orientation, no project-specific basic checks, nothing" is literally
accurate — there is nothing in the system that would produce it.

There is also nowhere to *keep* such findings. `memories` is scoped to operator
preferences ("Not task progress — that is the board") and starts empty. So even if
the agent oriented itself, it would re-do that work every fresh conversation.

### The prompt actively suppresses the depth you are asking for

Three passages work directly against your stated preference:

> **Be brief.** You are being read on a phone.

> Do the work that was asked and stop. Adding an abstraction nobody requested, a
> config option with one caller, or a feature "while you are in there" is how a
> small project becomes an unmaintainable one.

> Report only concrete, actionable findings. […] Be brief.  *(reviewer prompt)*

You told me extra token consumption is fine and you want it to go further —
proactive suggestions, deeper testing, edge cases, filling gaps you cannot fill
yourself. The prompt is tuned for the opposite tradeoff: minimum tokens, minimum
scope, minimum words. That is a coherent design; it is just not yours.

### Verification is named but not required

`loopRules` says "Verify before you claim" and lists the available checks
(project tests via bash, `http_check` against a `bash_bg` server, `read_logs`).
Nothing requires edge cases, nothing requires a test to be *added*, and there is
no spec or design artifact anywhere in the system — no template, no required
document, no format. So "spec-driven" currently has nothing to hang on.

### Leftover vocabulary from a deleted subsystem

The phase state machine was removed, but the model is still being handed its
vocabulary. `internal/tools/git.go`, `git_diff` description:

> With no arguments, shows uncommitted work against HEAD **during a phase**, or
> the whole **phase's** work when reviewing one.

and the `from` parameter: *"Defaults to the current **phase's** start commit."*

"Phase" appears nowhere in the prompt or the loop any more. The model is reading
tool documentation about a concept that does not exist. Cheap to fix, real source
of confusion.

### And it is fighting with one hand tied

Worth restating: per §2c the model loses its reasoning at every tool boundary. A
"careful, rule-following, spec-driven agent" is hard to get from a model that
cannot carry a chain of thought across a tool call. Prompt work will help; prompt
work *plus* reasoning continuity is a different tier.

---

## 6. Memories are write-only from your side

Confirmed by grep — there is **no read path in the web layer at all**.

- Stored as a single `project.memories` text column
  (`internal/store/scratchpad.go`, `Memories` / `SetMemories`).
- Written only by the `remember` tool, via `applyControl` in `agent.go`.
- Injected into the system prompt on every call (`prompt.go`).
- Never rendered anywhere. Not on Settings, not on Chat, not anywhere.

So the agent can rewrite its own durable memory, that memory shapes every single
turn, and you have no way to see it or correct it. For your context-drift concern
that is the worst possible arrangement.

**This is the cheapest fix in the whole report.** It is already one text column
with a working setter. A `<textarea>` in a collapsed Settings group plus one POST
handler is essentially the entire job. Same pattern would expose the task board if
you want it.

---

## Miscellaneous

**No step cap on the primary loop.** `Service.run` iterates until the model stops
requesting tools. So the primary agent "running out of turns" is really the model
*choosing* to stop — encouraged by "Do the work that was asked and stop" and "Be
brief", and made more likely by losing its reasoning each turn (§2c).

**No context management whatsoever.** `ContextMessages` returns every message with
`in_context` set, ordered by id. No trimming, no summarisation, no compaction. The
only lever is the destructive "Clear conversation". Given that Muse Spark is
documented as managing and compacting its own context, leaving this alone is
defensible — but there is no token-usage readout anywhere, so you are guessing
about where you actually are in the window. That guess is part of why it "feels"
exhausted. A usage indicator would settle it.

**Model default mismatch.** Config ships `muse-spark-1.2`; you run
`muse-spark-1.3-contributor`. Worth setting the default to what you use.

---

## The UI/UX subagent you asked for

Feasible, and the existing shape makes it cheap: `agent.runReview` is a working
template for a subagent — system prompt, own registry, own turn loop, triggered by
a tool. Both roles you described fit:

- **Design-schema advisor**, consulted *before* implementation, so the primary
  agent gets a design brief instead of inventing one.
- **UI guard**, running *after* the semantic reviewer, as a second layer.

One honest constraint you should decide on before we build it. The primary agent's
own prompt admits: *"You cannot see rendered pixels."* A UI subagent on the
current tooling has the same blindness — it would review markup and CSS
semantically and nothing more. That will catch some things, and it will not catch
the button that is 4px off.

The version that actually saves you "fix this button" sessions needs a screenshot
tool in the sandbox (headless browser → PNG) feeding images into a multimodal
call. Muse Spark does accept image input per the docs, so this is possible — but
it is a genuinely larger piece of work than the subagent itself. Worth deciding
deliberately rather than discovering halfway through.

---

## Suggested sequencing

**Round 1 — bugs, small, highest relief per line changed**
1. `max_tokens` → `max_completion_tokens`; clamp the setting to 131,072; replace
   the starvation message with one that reports what actually happened (§2a, §2b)
2. Reviewer turn budget + remaining-turns in the message + forced tools-free final
   turn (§1)
3. Strip the page reloads out of `chat.js`; reserve the agent-bar height; keep the
   streaming node mounted; one consistent scroll measurement (§3)
4. Memories editor on Settings (§6)

**Round 2 — behaviour**
5. Prompt rework: orientation pass, plan-by-default, spec/doc format, deeper
   verification and edge cases, remove the brevity ceiling; fix the "phase"
   vocabulary in tool descriptions (§5)
6. Reasoning continuity — Responses API or stateless reasoning replay (§2c). The
   biggest item here and the one that most changes how the agent feels.

**Round 3 — the UI you actually want to look at**
7. Collapse every settings group by default; wrap the trailing cards; strip
   teaching copy; one-line delete confirmation (§4)
8. Real icon set; PWA `id`/scope/splash/icon consistency (§4)

**Round 4**
9. UI/UX subagent (§ above), after deciding the screenshot question

Two decisions I need from you before Round 2: how far to go on the Responses API
migration, and whether we are building real screenshot capability.
