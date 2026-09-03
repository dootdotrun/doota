# doota — findings + fix roadmap

**Working doc.** Lives at root so it is easy to quote mid-flight. Delete it when
the roadmap is done. Full reasoning for every finding is in
`docs/diagnosis-2026-09.md`; this is the condensed, quotable version.

Quote items by ID: **F-numbers** are findings, **T-numbers** are work items.
Verified state at time of writing: `go build ./...` and `go vet ./...` both clean.

---

## Findings index

Severity: **S1** breaks the tool · **S2** degrades it badly · **S3** friction/polish

| ID | S | Finding | Where |
|---|---|---|---|
| **F1** | S1 | Reviewer budget is 5 *model calls*, not tool calls. Orient + 3 file reads = gone. Out-of-turns is the normal path. | `agent/review.go:18` |
| **F2** | S1 | Reviewer is never told its remaining budget, and is offered tools even on its final turn — so it spends the last turn on another tool request instead of a verdict. | `agent/review.go:89` |
| **F3** | S2 | Pre-ship guard checks review *attempted*, not passed. A reviewer that never concluded still satisfies it. Review step is decorative. | `store/history.go`, `agent/agent.go` ship() |
| **F4** | S1 | `Starved()` is a catch-all (`Silent() && !UsageReported`) reported as a specific diagnosis: "exhausted its output budget on reasoning". Any empty stream — dropped conn, filtered response — reads as budget exhaustion. | `model/model.go:171`, `agent/agent.go:330` |
| **F5** | S1 | Sends legacy `max_tokens`. Meta's Muse Spark config uses `max_completion_tokens`. Prime suspect for the 400. | `model/model.go:453` |
| **F6** | S1 | No upper bound on `model.max_output_tokens` (only `> 0`). Real ceiling is 131,072. F4's message *tells you to raise it* → past the ceiling → permanent 400, and Resume replays byte-identical. | `store/appconfig.go` ParseValue |
| **F7** | S1 | **Reasoning discarded every turn.** Chat Completions drops it; Meta recommends Responses API for tool loops *because* it preserves reasoning across turns. `message.reasoning` column + struct fields exist, never written, never read. | `model/model.go`, `migrations/001_init.sql:119` |
| **F8** | S3 | `model.go`'s central assumption was measured on **1.2**; running **1.3-contributor**. Config default still `muse-spark-1.2`. | `store/appconfig.go` |
| **F9** | S3 | `reasoning_effort` never sent. Accepts `minimal…xhigh`; most direct lever for "be more careful" and unused. | `model/model.go` params() |
| **F10** | S2 | `window.location.reload()` used as refresh mechanism on `run.state` / `plan.updated`. Destroys + rebuilds the EventSource each time → reconnect → `tail()`. This is the "auto refreshing entire page". | `static/chat.js` |
| **F11** | S2 | Streaming placeholder fights itself: deltas un-hide `#streaming`, any `tail()` calls `clearStream()` and hides it. Several times a second. This is the shake. | `static/chat.js` |
| **F12** | S2 | `#agent-bar` toggles `.hidden` and sits *above* the transcript → every state change shifts all content vertically. No space reserved. | `static/chat.js`, `templates/chat.html` |
| **F13** | S2 | `atBottom()` measures `document.body.offsetHeight`, `scroll()` targets `scrollHeight`. Guard and action disagree. | `static/chat.js` |
| **F14** | S3 | Reloads queue while composer has text/focus, then fire in a burst on blur. | `static/chat.js` |
| **F15** | S2 | **Memories have no read path in the web layer at all.** Agent rewrites its own durable memory, it shapes every turn, operator cannot see or correct it. | `store/scratchpad.go` |
| **F16** | S2 | Settings group collapses only if *every* field is a textarea → Credentials, Model, Git can never collapse. Four more sections (Sign-in, Session, Project ×2) sit outside the group loop, always open. | `web/settings.go` buildGroups |
| **F17** | S3 | Delete modal is ~50 words of explanation, every time. Teaching copy throughout: unconditional field help, chat empty state, plan blurb, sandbox messages. | `templates/layout.html`, `settings.html` |
| **F18** | S3 | Header icons: CSS is **correct** (`fill:none`, `stroke:currentColor`). Path data is hand-written stubs — gear is a circle plus 6 disconnected ticks. Needs an icon set, not a CSS fix. | `static/app.css:375`, `templates/layout.html` |
| **F19** | S3 | PWA: `id:"/"` vs `scope:"/app/"` mismatch; no splash config beyond white-on-white; maskable icon is RGB while others are RGBA. Why it feels like a different app. | `static/manifest.webmanifest` |
| **F20** | S2 | **Planning skipped by explicit instruction, in two places.** Not a bug — design aimed at a different operator. | `agent/prompt.go` loopRules, `tools/plan.go` |
| **F21** | S2 | **No orientation step exists.** Nothing instructs reading README / manifests / test commands on a fresh conversation. Nowhere to persist project facts either (`memories` is preferences-only). | `store/prompt.go`, `agent/prompt.go` |
| **F22** | S2 | Prompt actively suppresses requested depth: "Be brief, you are being read on a phone", "do the work that was asked and stop". Opposite of stated preference (tokens are fine, go deeper). | `store/prompt.go`, `agent/prompt.go` |
| **F23** | S2 | No spec/doc artifact anywhere — no template, no required format. "Spec-driven" has nothing to hang on. Verification named but edge cases never required. | `agent/prompt.go` |
| **F24** | S3 | Dead "phase" vocabulary in tool descriptions — subsystem was deleted, model still reads about it. | `tools/git.go` git_diff |
| **F25** | S3 | `agent.modelTimeout` 30min is shadowed by `model.streamTimeout` 10min. Dead and misleading. | `agent/agent.go`, `model/model.go` |
| **F26** | S3 | No context management and no token-usage readout. Only lever is destructive Clear. Part of why the window "feels" exhausted. | `store/messages.go` ContextMessages |

---

## Open decisions

- **D1 — Reasoning continuity scope (F7). ✅ DECIDED: full Responses API
  migration.** No half-measure on the Chat Completions path.

  One sub-decision follows from it and was taken on architectural grounds rather
  than asked: **stateless replay, not `previous_response_id`.** The Responses API
  offers both. `previous_response_id` keeps conversation state on Meta's servers,
  which would break the property the whole of this codebase is built on — that
  everything durable lives in Postgres and a restart resumes from the last
  boundary. It would also break Clear Conversation, which works by flipping
  `in_context`, and would tie the transcript to opaque remote ids that can expire
  under a run. So: `store: false`, `include: ["reasoning.encrypted_content"]`, and
  reasoning items replayed out of Postgres. The `message.reasoning` column that
  has existed and gone unused since the first migration is finally what carries it.

- **D2 — Real UI verification.** Build headless-browser screenshots in the sandbox
  feeding multimodal calls, or accept a markup/CSS-only UI subagent that is blind
  to rendered pixels like the primary agent already is? Affects **T4.2**. Still
  open; does not block anything before Turn 4.

---

## Turn 1 — bugs. Highest relief per line changed. ✅ DONE

No prompt changes, no visual redesign. Just stop the things that are actively
breaking.

- [x] **T1.1 — Model request correctness** (F5, F6, F4, F25)
  - Budget now travels as `max_completion_tokens`; `max_tokens` is gone
  - Clamped twice: `Field.Max` rejects >131072 at the form, and `params()` clamps
    on the wire so a pre-existing bad value cannot brick every request
  - Default raised 16384 → 65536 (headroom is nearly free; billing is on tokens
    generated, not on the ceiling)
  - `Starved()` replaced by `SilentCause()` → `truncated` / `reasoning` /
    `unknown`. Only the first two mention the budget; `unknown` says it is a
    dropped or rejected request and points at the model name and credentials
  - Dead 30-minute timeout removed; the stream cancel is now `WithCancel`, which
    is all Pause ever needed
- [x] **T1.2 — Reviewer can finish** (F1, F2)
  - Budget 5 → 24 model calls
  - Reviewer is told its remaining turns from 3 out
  - Final turn is made with **tools withheld**, which is what actually forces a
    verdict — warning alone just bought one more unanswered tool call
  - System prompt now tells it a budget exists and to always land a verdict
  - Silent responses reuse the same honest classification as T1.1
- [x] **T1.3 — UI stability** (F10, F11, F12, F13, F14)
  - New `fragments/controls.html` + `GET /chat/controls`. `run.state` and
    `plan.updated` swap ~40 lines instead of calling `window.location.reload()`
  - Composer deliberately left outside the fragment, so a draft survives every
    swap. Only its enabled state comes from the server, via `data-can-send`
  - Streamed placeholder is cleared **only** on `message.complete`, in the same
    DOM update as the append. `tail()` no longer clears it, which is what made a
    filling placeholder vanish and reappear several times a second
  - Agent bar always occupies its box; idle dims it (`.is-quiet`) instead of
    `display: none`, so the transcript stops jumping
  - One `docHeight()` used by both the at-bottom test and the scroll target
  - The whole reload-deferral mechanism is gone — nothing to defer
- [x] **T1.4 — Memories visible and editable** (F15)
  - Collapsed "Memories" group on Settings + `POST /settings/memories`, reusing
    the existing `SetMemories`. Capped at 8000 to match the agent's own limit

Verified: `go build` + `go vet` clean; all 12 run-state shapes render through the
new fragment; `max_tokens` confirmed absent from the wire and the ceiling
confirmed clamped, via throwaway tests (removed — say the word and I will add
them back as permanent regression guards).

Picked up opportunistically while in the same markup (was T3.2): chat empty-state
card, the plan-approval explainer, and `AwaitingDetail` in the agent bar are gone.

Deliberately **not** in Turn 1: F3 (needs a decision about whether `done` should
hard-block on an inconclusive review — touches behaviour, belongs with the prompt
work).

## Turn 2 — behaviour. The "it feels generic" turn.

Transport work (T2.3–T2.5) is done. Prompt work (T2.1–T2.2, T2.6) is next.

- [ ] **T2.1 — Prompt rework** (F20, F21, F22, F23)
  - Orientation pass on fresh conversations: read README, manifests, discover test
    and build commands, note conventions
  - Plan-and-approve becomes the **default**, with a fast lane for genuine
    questions
  - Introduce a spec/doc format the agent must produce and follow
  - Require edge cases and deeper verification; remove the brevity ceiling
  - Somewhere to persist project facts across conversations
- [ ] **T2.2 — Vocabulary cleanup** (F24) — remove dead "phase" language
- [x] **T2.3 — Reasoning continuity** (F7, F5) — **done.** Full migration to the
      Responses API, stateless.
  - `internal/model` now speaks `/v1/responses`. `messages` → `input` items,
    system prompt → `instructions`, budget → `max_output_tokens`
  - `store: false` + `include: ["reasoning.encrypted_content"]`, so the reasoning
    blobs come back to us instead of living on Meta's servers
  - Migration `002`: the unused `message.reasoning` text column is dropped and
    replaced by `reasoning_items jsonb`. Reasoning items are persisted with the
    assistant turn that produced them and replayed verbatim on later requests
  - Item ordering within an assistant turn is reasoning → text → tool calls, so
    the thinking precedes the decision it produced
  - The reviewer gets continuity too, which matters much more now it has 24 turns
  - Bonus, and it fixes the rest of F4: truncation is no longer inferred. The API
    reports `incomplete_details.reason`, so `truncated` and a new `filtered` are
    both trustworthy, and usage now breaks out `reasoning_tokens` explicitly
- [x] **T2.4 — Expose `reasoning_effort`** (F9) — new `KindChoice` field kind
      (validated select, not free text, so it cannot store a value the API
      rejects). Blank = let the model decide. Applies to the agent and reviewer
- [x] **T2.5 — Default to `muse-spark-1.3-contributor`** (F8); the 1.2-era
      measurements in `model.go` are rewritten around this transport
- [ ] **T2.6 — Decide the `done` / review gate** (F3)

## Turn 3 — the UI you want to look at.

- [ ] **T3.1 — Settings** (F16) — every group collapsed by default; wrap the four
      trailing sections
- [ ] **T3.2 — Strip teaching copy** (F17) — one-line delete confirm; help text
      behind a disclosure or gone
- [ ] **T3.3 — Real icon set** (F18) — inline SVG, no dependency
- [ ] **T3.4 — PWA identity** (F19) — `id`/scope, splash, icon consistency
- [ ] **T3.5 — Token usage readout** (F26) — stop guessing about the window

## Turn 4 — the UI/UX subagent.

- [ ] **T4.1 — Subagent** — `agent.runReview` is the working template. Two roles:
      design-schema advisor consulted *before* implementation, and UI guard
      running *after* the semantic reviewer
- [ ] **T4.2 — Real verification** — **blocked on D2**

---

## Standing constraints

Things to keep true while fixing the above.

- **Stay monolith.** All UI instability is client-side; none of it is caused by the
  server. Server-owned rendering with one message-bubble implementation is the
  good part of this architecture — keep it. Revisit decoupling only if something
  actually forces it.
- **Least-cluttered UI is a hard requirement, not a preference.** Default to
  collapsed, default to silent, assume the operator knows what the button does.
- **Token cost is not a constraint.** Depth, proactive suggestions, and
  self-directed edge-case testing are wanted. Optimise for care, not brevity.
- **The operator is non-technical.** Anything that needs a judgement call about
  implementation is the agent's job to raise, not the operator's job to know.
