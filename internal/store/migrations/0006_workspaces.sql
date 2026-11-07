-- Workspaces: a development environment that outlives any one request.
--
-- The longest-lived entity used to be a task, born from a request and dead at
-- the merge gate, so there was nothing meaning "the environment I come back
-- to". That is the shape a batch orchestrator needs and the wrong one for
-- working on something: you want a machine with your branch on it, your
-- services running, and a preview you can keep looking at.
--
-- A fork becomes a workspace created by a plan, on a scratch branch, with a
-- lifecycle that ends at merge. Keeping one record for both is what stops
-- provisioning, service startup and preview allocation being written twice.
CREATE TABLE workspaces (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    repo_id      TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    project_id   TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    branch       TEXT NOT NULL,
    state        TEXT NOT NULL,
    state_reason TEXT NOT NULL DEFAULT '',
    instance_id  TEXT NOT NULL DEFAULT '',
    preview_url  TEXT NOT NULL DEFAULT '',
    -- Set for a workspace a plan created to run one fork, so cleanup can tell
    -- scratch space apart from somewhere a person works.
    ephemeral    INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    started_at   TEXT,
    stopped_at   TEXT,
    last_used_at TEXT
) STRICT;

CREATE INDEX idx_workspaces_user ON workspaces (user_id, state);
CREATE INDEX idx_workspaces_repo ON workspaces (repo_id);

-- A fork now runs in a workspace rather than owning a machine directly.
-- Nullable: forks created before this exist without one, and a fork that never
-- reached provisioning never had a machine to put in it.
ALTER TABLE forks ADD COLUMN workspace_id TEXT REFERENCES workspaces (id);

CREATE INDEX idx_forks_workspace ON forks (workspace_id);
