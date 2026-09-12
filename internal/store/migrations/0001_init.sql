-- Initial dabberz control-plane schema.
--
-- Every entity is scoped by user_id even though v1 is single-user, so that
-- multi-user support is additive rather than a migration of every table.

CREATE TABLE users (
    id         TEXT PRIMARY KEY,
    email      TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
) STRICT;

-- A repo is the root of the secrets, memory and config hierarchy.
CREATE TABLE repos (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    remote_url     TEXT NOT NULL,
    default_branch TEXT NOT NULL,
    discovered     INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    UNIQUE (user_id, name)
) STRICT;

-- One buildable unit inside a repo; monorepos yield several.
CREATE TABLE projects (
    id              TEXT PRIMARY KEY,
    repo_id         TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    path            TEXT NOT NULL,
    toolchain       TEXT NOT NULL DEFAULT '',
    preview_command TEXT NOT NULL DEFAULT '',
    preview_port    INTEGER NOT NULL DEFAULT 0,
    confirmed       INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    UNIQUE (repo_id, path)
) STRICT;

CREATE TABLE tasks (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    repo_id            TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    title              TEXT NOT NULL,
    request            TEXT NOT NULL,
    state              TEXT NOT NULL,
    merge_target       TEXT NOT NULL,
    merge_timing       TEXT NOT NULL,
    integration_branch TEXT NOT NULL DEFAULT '',
    plan_json          TEXT,
    state_reason       TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
) STRICT;

CREATE INDEX idx_tasks_user_state ON tasks (user_id, state);
CREATE INDEX idx_tasks_repo ON tasks (repo_id);

-- A fork is one independent workstream owning exactly one VM.
CREATE TABLE forks (
    id              TEXT PRIMARY KEY,
    task_id         TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    user_id         TEXT NOT NULL,
    repo_id         TEXT NOT NULL,
    project_id      TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    branch          TEXT NOT NULL,
    state           TEXT NOT NULL,
    state_reason    TEXT NOT NULL DEFAULT '',
    serialize_group TEXT NOT NULL DEFAULT '',
    instance_id     TEXT NOT NULL DEFAULT '',
    preview_url     TEXT NOT NULL DEFAULT '',
    usage_json      TEXT NOT NULL,
    escalation_json TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    started_at      TEXT,
    ended_at        TEXT
) STRICT;

CREATE INDEX idx_forks_task ON forks (task_id);
CREATE INDEX idx_forks_state ON forks (state);
CREATE INDEX idx_forks_repo_state ON forks (repo_id, state);

-- Append-only activity feed, powering both the audit trail and the web UI.
-- seq, not id, is the stream cursor: ids only sort chronologically to
-- millisecond precision, so two events written in the same millisecond have no
-- defined order between them. AUTOINCREMENT gives the feed a strictly
-- monotonic cursor that a reconnecting client can resume from exactly.
CREATE TABLE events (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    id         TEXT NOT NULL UNIQUE,
    user_id    TEXT NOT NULL,
    task_id    TEXT,
    fork_id    TEXT,
    type       TEXT NOT NULL,
    message    TEXT NOT NULL DEFAULT '',
    data_json  TEXT,
    created_at TEXT NOT NULL
) STRICT;

CREATE INDEX idx_events_task ON events (task_id, seq);
CREATE INDEX idx_events_fork ON events (fork_id, seq);
CREATE INDEX idx_events_user ON events (user_id, seq);

-- Published preview routes. The hostname is unique across the machine because
-- a single Caddy instance fronts every fork.
CREATE TABLE preview_routes (
    fork_id       TEXT PRIMARY KEY REFERENCES forks (id) ON DELETE CASCADE,
    hostname      TEXT NOT NULL UNIQUE,
    upstream_host TEXT NOT NULL,
    upstream_port INTEGER NOT NULL,
    created_at    TEXT NOT NULL
) STRICT;

CREATE INDEX idx_preview_routes_port ON preview_routes (upstream_port);

-- Repo-scoped secrets, encrypted at rest. Values are inherited by every
-- project and fork under the repo.
CREATE TABLE secrets (
    id         TEXT PRIMARY KEY,
    repo_id    TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    nonce      BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (repo_id, name)
) STRICT;
