-- Services: a project runs a set of processes, not a single dev server.
--
-- The projects table used to carry one preview_command and one preview_port,
-- which could describe "a React app with a dev server" and nothing else. A
-- project that is a web app plus an API plus a worker plus a database could
-- not be expressed at all. 0004_backfill_services.go migrates the old columns
-- into this table and then drops them, so there is never a period with two
-- sources of truth for how a project runs.
CREATE TABLE services (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    command     TEXT NOT NULL DEFAULT '',
    port        INTEGER NOT NULL DEFAULT 0,
    health_path TEXT NOT NULL DEFAULT '',
    exposed     INTEGER NOT NULL DEFAULT 0,
    is_primary  INTEGER NOT NULL DEFAULT 0,
    -- JSON array of service names within the same project.
    depends_on  TEXT NOT NULL DEFAULT '[]',
    confirmed   INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE (project_id, name)
) STRICT;

-- At most one front door per project, enforced here rather than left to
-- application code that could be bypassed by a direct write.
CREATE UNIQUE INDEX idx_services_primary ON services (project_id) WHERE is_primary = 1;

CREATE INDEX idx_services_project ON services (project_id);
