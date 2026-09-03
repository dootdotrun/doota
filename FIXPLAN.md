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
| **F27** | S2 | **Plan progress counter never moved.** `loadPlan` counted tasks with status `"complete"`; the four real statuses are `pending`/`doing`/`done`/`blocked`. So every plan read `0/N` for its whole life. Found while reading the file for T2.1, not in the original audit. | `web/screens.go` loadPlan |

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

- **D2 — Real UI verification. ✅ DECIDED: headless browser and real screenshots.**
  A subagent reading markup and CSS is not worth building; the whole point is to
  catch what only shows up once rendered.

  Design notes that follow from it:

  - **Chromium runs in the sandbox, not here.** The dev server the agent starts is
    on `localhost:<port>` inside the sandbox, so that is where the browser has to
    be. It also keeps the app's own machine free of a browser install.
  - **Screenshots come back through `Sandbox.ReadFile`**, not base64 through
    stdout. `bash` output is capped at 30KB and a screenshot is an order of
    magnitude larger; `ReadFile` already exists and has no such cap.
  - **Only the subagent sees the images.** A tool result on the Responses API is a
    string, so the primary agent cannot be handed a picture — it gets the
    reviewer's written findings. That is the division asked for anyway: the primary
    agent starts the server and says where it is, the subagent looks.
  - **Viewport captures, not full-page.** Full-page needs CDP; `--screenshot`
    captures the viewport. Phone and desktop viewports are what "does this button
    look right" actually depends on, and the tool says plainly that it is
    viewport-only rather than pretending otherwise.
  - **Self-healing install.** The setup script installs Chromium, but it only runs
    at project creation, so existing sandboxes have none. The tool detects that and
    says exactly how to fix it instead of failing opaquely.

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

## Turn 2 — behaviour. The "it feels generic" turn. ✅ DONE

- [x] **T2.1 — Prompt rework** (F20, F21, F22, F23)
  - **Orientation now exists.** New `record_orientation` tool + `project.orientation`
    column (migration 003). The loop reads the README, manifests, test/lint config
    and CI, works out the real build/test/run commands, *runs them to check*, and
    records what it found. Injected into the prompt every call, so its absence is
    itself the instruction to go and orient. Kept separate from `memories`: that is
    what the operator said, this is what the agent worked out — different lifetimes,
    different authorities
  - **Plan-and-approve is the default.** `create_plan` is now the normal way to
    start any change, not something to be asked for. Only exception is the operator
    explicitly saying skip it. Questions are still answered as questions
  - **A spec format that exists and is enforced at the tool boundary.**
    `create_plan` requires problem, approach, and verification, and prompts hard for
    edge cases, risks, and open questions. Rendered *open* on the approval card —
    the one place in this UI where more text is correct, because it is the only
    moment a non-programmer can catch a misunderstanding. Shown back to the model
    every turn so "verify what you agreed" has something specific to check against
  - **Brevity ceiling removed, deliberately not by deleting scope discipline.**
    "Do the work that was asked and stop" stays — that is good engineering. What is
    added is a requirement to report anything noticed-but-not-done as a
    recommendation. Proactive suggestions without scope creep. And brevity is now
    scoped to prose: "be brief in what you say and exhaustive in what you check"
  - **The prompt now knows who it works for.** Non-technical operator, agent is the
    last line of review, thoroughness costs nothing, never hand over a technical
    decision without a plain-language trade-off and a recommendation, never claim
    done without saying how you know
- [x] **T2.2 — Vocabulary cleanup** (F24) — `git_diff`'s description and parameter
      help no longer refer to "phases", a subsystem deleted before this audit. The
      remaining mentions are Go comments recording why it was removed, which is
      history worth keeping and is not model-facing
- [x] **F27 — Plan progress counter fixed.** Compared against `"complete"`; the real
      constant is `"done"`. Found while editing `loadPlan` for the spec view
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
- [x] **T2.6 — The `done` / review gate** (F3) — **decided: require a verdict, not
      an attempt.** New `ReviewOutcomes` distinguishes a review that concluded from
      one that merely happened. `done` refuses if no review was attempted, and
      refuses again if one was attempted but never reached a verdict.

      It deliberately cannot trap the run: after two attempts the agent is let
      through on condition it states the review was inconclusive rather than
      describing the work as reviewed. A reviewer broken in a way the operator
      cannot repair from this UI should not be able to hold work hostage — but it
      should not be able to launder it either.

## Turn 3 — the UI you want to look at. ✅ DONE

- [x] **T3.1 — Settings** (F16) — everything closed on arrival. The old rule
      ("collapsed only if every field is a textarea") could never close Credentials,
      Model or Git; the four trailing sections — Sign-in, Session, and the whole
      Project block — were not in the group loop at all and were always open. All of
      it is `<details>` now, all closed.

      One conditional exception, because a rule with no exception here would be
      worse: a group holding an **unset required credential** opens, since the banner
      at the top sends you there to fill it in. Same for the create-project form when
      there is no project, and Sign-in while still on the default password. Nothing
      else opens itself.
- [x] **T3.2 — Teaching copy cut** (F17)
  - Delete confirmation: two paragraphs → "Clear this conversation?" + two buttons
  - Field help trimmed at the source, in `ConfigFields`, not hidden in the template
    — "Covers reasoning as well as visible output, and reasoning comes first.
    Maximum 131072." became "Includes reasoning. Max 131072." Two fields lost their
    help entirely because the label already said it
  - The repo-URL, preview, setup-script and sandbox-recovery paragraphs are one
    short line each. `sandboxBlockedMessage` went from a sentence-and-a-clause per
    status to a fragment
- [x] **T3.3 — Real icon set** (F18) — new `fragments/icons.html`, defined once and
      shared, so the enabled and disabled variants of each header button stop
      carrying duplicate path data.

      The settings glyph is now **sliders, not a gear**: a gear needs eight teeth to
      read as a gear and eight teeth at 19px is mud. The old one was a circle with
      six disconnected tick marks around it. Reload, clear and preview are redrawn
      with geometry that survives 19px; send is unchanged in meaning.
- [x] **T3.4 — PWA identity** (F19)
  - `id` was `/` while `scope` and `start_url` were `/app/`. Now all `/app/`
  - `background_color` was `#ffffff` on a white `theme_color`, so both platforms
    generated a blank white splash. Background is now the accent blue
  - **Icons regenerated** to match the app: accent blue, two white dots for the
    "oo" in doot, antialiased by 4x supersampling in a throwaway Go generator (no
    image library needed). Maskable variant is full-bleed with the mark inside the
    safe zone
  - *Correction to F19:* the maskable icon being RGB rather than RGBA is **not** a
    defect. A maskable icon must be fully opaque, so PNG drops the alpha channel
    correctly. The original finding overstated that one
- [x] **T3.5 — Context readout** (F26) — migration 004 adds `prompt_tokens` and
      `reasoning_tokens` to `message`; `token_count` only ever held completion
      tokens, which answers what a reply cost and not how full the conversation is.
      The footer now reads e.g. `muse-spark-1.3-contributor · 250k / 1048k · 23%`,
      taken from the API's own accounting on the last call rather than estimated.

## Turn 4 — the UI/UX subagent, with eyes. ✅ DONE

Built bottom-up: capability first, then the agent that uses it, then the gate that
makes it non-optional.

- [x] **T4.1 — Image input in `internal/model`.** `Message.Images` carries raw bytes
      (not pre-built data URLs, so one place knows the encoding) and becomes
      `input_image` content parts at detail `high` — low detail is exactly what would
      smooth away the misalignment and clipping worth catching. Refused before
      sending if empty or over 5MB, since that returns as an opaque payload error
- [x] **T4.2 — `screenshot` tool.** Headless Chromium **inside the sandbox**, because
      the dev server it photographs is on `localhost:<port>` in there. Bytes come back
      via `Sandbox.ReadFile`, not base64 through stdout — command output is capped at
      30KB and every capture would have been silently truncated into a corrupt PNG.
      `--no-sandbox` and `--disable-dev-shm-usage` because Chromium's own sandbox
      needs privileges a container lacks and `/dev/shm` is usually tiny.
      Labels are sanitised, because a label becomes a filename and `../` would write
      outside the capture directory
- [x] **T4.3 — The subagent** (`agent/uireview.go`), modelled on `runReview`: own
      history, own registry, budget of 20, warning at 3, **tools withheld on the last
      turn** so a verdict always arrives. Captures phone (390×844) and desktop
      (1440×900). A screenshot it takes mid-review is attached as a following user
      turn, because a tool result on this transport is text
- [x] **T4.4 — `ui_review` control tool**, executed by the runner exactly like
      `review`. Two modes: `design` before building (returns a brief with real units,
      states, and responsive behaviour), `verify` after (returns defects or CLEAN)
- [x] **T4.5 — The gate.** `done` requires a concluded `ui_review` when the diff
      touches files that can *look* wrong without *being* wrong — templates,
      stylesheets, component files. Deliberately not every front-end file: a lockfile
      cannot move a button, and a guard that fires on everything is one to resent.
      Non-trapping on the same terms as T2.6
- [x] **T4.6 — Chromium in the setup script** (POSIX-safe, reported separately in the
      provision log because a missing browser disables one feature rather than
      breaking something general), plus a transcript card that distinguishes a design
      brief from a verify verdict

Two things found while building this, both fixed:

- **`screenshot` validated its arguments after looking up the sandbox**, so a missing
  label surfaced as "no sandbox available" — an infrastructure error, which strands
  the whole run, for something the model could have corrected from a one-line result.
  Arguments are checked first now.
- **Migration 005 finds the old `kind` CHECK constraint in the catalog** rather than
  dropping it by name. It was declared inline in `CREATE TABLE`, so Postgres named it;
  a `DROP CONSTRAINT IF EXISTS` guessing wrong would have succeeded silently and left
  the original still rejecting `ui_review`. The symptom would have been a run stranded
  on its first UI review, a long way from the cause.

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
