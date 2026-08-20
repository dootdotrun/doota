# doot

A single-user, single-project autonomous coding agent. Give it a goal from your
phone, approve a plan, and it works through the plan in a persistent cloud
sandbox — committing, testing, and pushing to a branch called `doot`.

Go · Postgres · Fly Sprites sandboxes · Fly.io · installable PWA

## Deploying

One environment variable. Everything else is configured in the app.

| Variable | Required | Notes |
| --- | --- | --- |
| `DATABASE_URL` | **yes** | Postgres connection string. `NEON_CONNECTION_STRING` is also accepted. |
| `PORT` | no | Defaults to `8080`, which is what `fly.toml` expects. |
| `DOOT_SANDBOX_PROVIDER` | no | `sprites` (default) or `local`. Never set `local` in a deployment — it gives the agent this container's filesystem instead of an isolated VM. |
| `DOOT_LOCAL_SANDBOX_DIR` | no | Only used by the `local` provider. |

From the Fly.io dashboard:

1. Launch an app from this GitHub repository. Fly builds the `Dockerfile` in the
   repository root with its own remote builder — no flyctl, no local Docker.
2. Name the app `doot`, or edit `app` in `fly.toml` to match the name you chose.
3. Set `DATABASE_URL` as a secret. If you attach Fly Postgres from the dashboard
   it sets this for you.
4. Enable auto-deploy on push if you want it. It needs no configuration here.

Then open the app and finish setup in the UI:

1. Sign in with `doot` / `doot`.
2. **Settings → Credentials**: model API key, Fly Sprites token, GitHub token
   (a PAT with `repo` scope — it is the only GitHub credential, used for clone,
   fetch, push, and pull requests).
3. **Settings → Model**: model id and the OpenAI-compatible base URL.
4. **Settings → Sign-in**: change the password off `doot` / `doot`.
5. **Project**: create the project with a name and a repository URL.

Until the credentials are set, the app boots and says what is missing rather than
refusing to start.

## Why configuration lives in the database

Credentials used to be environment variables, which meant seven secrets in the
Fly dashboard and a redeploy to change a typo in any of them. They are now rows
in `app_config`, edited on the Settings screen:

- **One secret to deploy.** The database URL is the only thing the process cannot
  discover for itself.
- **No redeploy to fix a credential.** A wrong key is corrected in a form field
  and applies to the next model call, the next sandbox operation, and the next
  push. Nothing is captured at boot.
- **Reads are cached.** Configuration is now on hot paths — every agent step,
  tool call, and page render — so a snapshot is held in memory and refreshed only
  when a write invalidates it or after a 30-second ceiling. In practice a burst
  of page loads causes zero queries.
- **Secrets are write-only in the UI.** A stored credential is never rendered
  back to the browser; the form shows only whether it is set, and submitting an
  empty field leaves the stored value alone.
- **The session key is generated once and persisted**, so a deploy no longer
  signs you out.

If the retired variables (`LLM_API_KEY`, `LLM_API_ENDPOINT`, `LLM_MODEL`,
`SPRITE_TOKEN`, `GITHUB_TOKEN`, `SESSION_SECRET`) are still present, they seed
the matching setting once on first boot and can then be deleted.

### What that costs

Credentials are stored in plaintext `jsonb` in `app_config`, alongside the cookie
signing key. Anyone holding `DATABASE_URL` — or a dump, or a database branch —
holds the model key, the Sprites token, the GitHub PAT, and the ability to forge a
session. There is no envelope encryption, because the key to do it with would have
to live in the environment, which is the thing being removed.

That is an accepted tradeoff for a single-operator internal tool whose alternative
was seven secrets in a hosting dashboard and a redeploy to fix a typo in any of
them. It would not be the right call for a multi-tenant deployment. Treat the
database URL as the master credential it now is, and use a GitHub PAT scoped to
the repositories this tool is meant to touch.

## The UI

Built for one person on a phone. Three tabs — **Chat**, **Project**,
**Settings** — because the previous five meant crossing four screens to make one
small change. The plan and its approve/revise buttons are on the Chat screen, in
the conversation they belong to; the read-only history is one disclosure away
under Project.

Light only, monospace throughout, white/off-white/charcoal with blue, green, and
red as the only accents. Agent responses are rendered as markdown, sanitised
server-side through an explicit allowlist — no raw HTML, and no images, since that
prose is assembled from repository files and fetched pages.

In the composer, Enter always inserts a newline and the box grows to 40% of the
viewport before it scrolls; sending is the button, or Ctrl/Cmd+Enter. That is
unconditional rather than branched on a touch-detection media query: when such a
query is wrong the failure is silent, and it lands on the one device this is
built for.
