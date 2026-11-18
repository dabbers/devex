-- Changes: a proposed change to an installation.
--
-- The record behind the sentence the product is selling. Somebody asks for
-- something in English, sees it running somewhere that is not their live site,
-- and decides whether it goes live. The branch, the preview, the checks and
-- the promote are one thing from the customer's side, so they are one row.
--
-- The preview has its own database, restored from a snapshot of the real one.
-- An agent cannot destroy a customer's data by experimenting, because what it
-- can reach is not the customer's data.
CREATE TABLE changes (
    id              TEXT PRIMARY KEY,
    installation_id TEXT NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
    user_id         TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    title           TEXT NOT NULL,
    branch          TEXT NOT NULL,
    workspace_id    TEXT REFERENCES workspaces (id) ON DELETE SET NULL,
    -- The scratch copy, dropped when the change is finished with.
    database        TEXT NOT NULL DEFAULT '',

    state           TEXT NOT NULL,
    state_reason    TEXT NOT NULL DEFAULT '',

    preview_url     TEXT NOT NULL DEFAULT '',
    -- The critical-path results, as JSON. A list the customer reads rather
    -- than queries, and one that belongs to exactly one change.
    checks          TEXT NOT NULL DEFAULT '[]',
    revision        TEXT NOT NULL DEFAULT '',

    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    promoted_at     TEXT
) STRICT;

CREATE INDEX changes_by_installation ON changes (installation_id, created_at DESC, id DESC);

-- What is deployed, and what a rollback returns to. One deep, deliberately: a
-- customer rolling back wants the thing that worked ten minutes ago, and a
-- list of twenty revisions is a decision they cannot make.
ALTER TABLE installations ADD COLUMN revision TEXT NOT NULL DEFAULT '';
ALTER TABLE installations ADD COLUMN previous_revision TEXT NOT NULL DEFAULT '';
