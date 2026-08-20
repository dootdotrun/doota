-- doot-ai schema.
--
-- This is the whole schema in one file rather than an evolution history. The
-- project had four migrations before it was ever deployed: they described how the
-- design changed during development, not anything a running database had to be
-- walked through. Tables that were created and then dropped, and columns added and
-- then removed, are simply absent.
--
-- Design notes that matter:
--   * History is append-only. Messages are never deleted; in_context controls
--     what the model sees and archived_at records when it stopped seeing it.
--   * The working state of a plan is one JSONB column on project, not a tree of
--     goal and phase tables. It is read and written as a unit by one runner.
--   * The project is a soft-deleted singleton, so deleting a project keeps its
--     conversation and tool history readable.
--   * Hard product limits (one project, one active run, branch "doot") are
--     enforced here rather than in application code.

-- ---------------------------------------------------------------------------
-- users
-- ---------------------------------------------------------------------------
CREATE TABLE users (
    id                       uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    username                 text        NOT NULL UNIQUE,
    password_hash            text        NOT NULL,
    password_change_required boolean     NOT NULL DEFAULT false,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- app_config: non-secret, runtime-editable configuration
-- ---------------------------------------------------------------------------
CREATE TABLE app_config (
    key        text        PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- project: at most one active row, ever
--
-- scratchpad is the task board: {title, status, base_commit, feedback, tasks[]}.
-- memories is the operator's durable conventions and preferences, injected into
-- the system prompt on every call.
-- ---------------------------------------------------------------------------
CREATE TABLE project (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    singleton      boolean     NOT NULL DEFAULT true CHECK (singleton),
    name           text        NOT NULL,
    repo_url       text        NOT NULL,
    default_branch text        NOT NULL DEFAULT 'main',
    work_branch    text        NOT NULL DEFAULT 'doot' CHECK (work_branch = 'doot'),
    sandbox_id     text,
    sandbox_status text        NOT NULL DEFAULT 'provisioning'
        CHECK (sandbox_status IN ('provisioning','ready','sleeping','error','missing')),
    preview_port   integer     NOT NULL DEFAULT 3000,
    setup_log      text,
    scratchpad     jsonb       NOT NULL DEFAULT '{}',
    memories       text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz
);

-- Only one non-deleted project can exist. Deleted rows are exempt so history
-- accumulates across project re-creations.
CREATE UNIQUE INDEX project_singleton ON project (singleton) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- run: the agent loop's durable state
--
-- No lease columns. One machine drives one project, so there is nothing to hand
-- a run over to; a run left running by a restart is picked up at boot.
-- ---------------------------------------------------------------------------
CREATE TABLE run (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       uuid        NOT NULL REFERENCES project(id),
    state            text        NOT NULL DEFAULT 'idle'
        CHECK (state IN ('idle','running','awaiting_human','paused','done','failed')),
    awaiting_reason  text
        CHECK (awaiting_reason IN ('plan_approval','question','error')),
    awaiting_payload jsonb,
    pause_requested  boolean     NOT NULL DEFAULT false,
    error            text,
    step_count       integer     NOT NULL DEFAULT 0,
    started_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz
);

-- At most one live run per project. Two runs driving the same sandbox and branch
-- would interleave commits.
CREATE UNIQUE INDEX run_one_active ON run (project_id)
    WHERE state IN ('running','awaiting_human','paused');

CREATE INDEX run_resumable ON run (state);

-- ---------------------------------------------------------------------------
-- message: the transcript, append-only
-- ---------------------------------------------------------------------------
CREATE TABLE message (
    id           bigserial   PRIMARY KEY,
    project_id   uuid        NOT NULL REFERENCES project(id),
    run_id       uuid        REFERENCES run(id),
    role         text        NOT NULL CHECK (role IN ('system','user','assistant','tool')),
    kind         text        CHECK (kind IN ('plan','review','ask_human','notice')),
    content      text        NOT NULL DEFAULT '',
    reasoning    text,
    tool_calls   jsonb,
    tool_call_id text,
    tool_name    text,
    token_count  integer,
    in_context   boolean     NOT NULL DEFAULT true,
    archived_at  timestamptz,
    interrupted  boolean     NOT NULL DEFAULT false,
    tool_display jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Hot path: building the model context window.
CREATE INDEX message_context ON message (project_id, id) WHERE in_context;
CREATE INDEX message_project ON message (project_id, id);
CREATE INDEX message_run     ON message (run_id);

-- ---------------------------------------------------------------------------
-- tool_call_log: forensic record, survives a cleared conversation
-- ---------------------------------------------------------------------------
CREATE TABLE tool_call_log (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id         uuid        REFERENCES run(id),
    message_id     bigint      REFERENCES message(id),
    tool_name      text        NOT NULL,
    args           jsonb,
    result_content text,
    result_display jsonb,
    is_error       boolean     NOT NULL DEFAULT false,
    duration_ms    integer,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX tool_call_run  ON tool_call_log (run_id, created_at);
CREATE INDEX tool_call_name ON tool_call_log (tool_name);

-- ---------------------------------------------------------------------------
-- background_process: dev servers and watchers
--
-- Persisted rather than held in memory because the sandbox outlives this process:
-- a redeploy replaces the machine but leaves the sandbox and everything running
-- inside it untouched.
-- ---------------------------------------------------------------------------
CREATE TABLE background_process (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid        NOT NULL REFERENCES project(id),
    name       text        NOT NULL,
    command    text        NOT NULL,
    cwd        text,
    log_path   text        NOT NULL,
    status     text        NOT NULL DEFAULT 'running'
        CHECK (status IN ('running','stopped','unknown')),
    started_at timestamptz NOT NULL DEFAULT now(),
    stopped_at timestamptz
);

CREATE UNIQUE INDEX bg_active_name ON background_process (project_id, name)
    WHERE status = 'running';

-- ---------------------------------------------------------------------------
-- event: SSE transport. The one prunable table.
-- ---------------------------------------------------------------------------
CREATE TABLE event (
    id         bigserial   PRIMARY KEY,
    project_id uuid        NOT NULL REFERENCES project(id),
    run_id     uuid        REFERENCES run(id),
    type       text        NOT NULL,
    payload    jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX event_stream  ON event (project_id, id);
CREATE INDEX event_created ON event (created_at);
