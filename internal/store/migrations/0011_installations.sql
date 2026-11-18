-- Installations: one customer's copy of one app.
--
-- The platform contract draws the line this table exists to hold. An
-- installation is the product somebody is using; a workspace is where a change
-- to it is made. A change is worked on in a workspace, previewed, checked and
-- promoted, and the installation is what the promote deploys to. Different
-- lifetimes and different audiences, so different records -- even though both
-- end up as services running on a machine.
--
-- The connection string is deliberately absent. Only the database name is
-- here, so this row can be read, logged and returned over the API without
-- carrying a credential; the DSN is sealed in the vault with everything else
-- the app is started with.
CREATE TABLE installations (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    app          TEXT NOT NULL,
    -- Pinned, and moved only by an upgrade. An app that changed underneath a
    -- customer because upstream shipped is the thing hosting is meant to
    -- protect them from.
    app_version  TEXT NOT NULL,
    name         TEXT NOT NULL,

    state        TEXT NOT NULL,
    state_reason TEXT NOT NULL DEFAULT '',

    repo_id      TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    project_id   TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    -- Set once an environment is serving it. Cleared when stopped, because the
    -- workspace may be rebuilt while the installation and its data persist.
    workspace_id TEXT REFERENCES workspaces (id) ON DELETE SET NULL,

    database     TEXT NOT NULL DEFAULT '',
    url          TEXT NOT NULL DEFAULT '',
    domain       TEXT NOT NULL DEFAULT '',

    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    started_at   TEXT
) STRICT;

-- The listing every account sees, newest first.
CREATE INDEX installations_by_user ON installations (user_id, created_at DESC, id DESC);

-- One account may install an app more than once, but not twice under the same
-- name: a form for recruiting and a form for support are two installations,
-- and telling them apart is the whole point of the name.
CREATE UNIQUE INDEX installations_name_per_user ON installations (user_id, name);
