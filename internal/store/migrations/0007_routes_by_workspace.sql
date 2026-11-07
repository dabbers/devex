-- Preview routes belong to a workspace, not to a fork.
--
-- The machine and the services on it belong to the workspace; a route points
-- at a service on that machine. A fork's preview URL is its workspace's. The
-- column held a fork id only because forks were the sole thing that had a
-- machine, and leaving it named that way would mean every reader has to know
-- that "fork_id" sometimes holds a workspace id -- the kind of lie that is
-- cheap to tell now and expensive later.
--
-- Rows are dropped rather than migrated. A route points at a port on a machine
-- that a fork from before this change is no longer connected to through a
-- workspace, so there is nothing truthful to re-key them to, and a published
-- route nothing can explain is worse than no route: the allocator reissues one
-- the next time the workspace starts.
DROP TABLE preview_routes;

CREATE TABLE preview_routes (
    workspace_id  TEXT NOT NULL,
    service_id    TEXT NOT NULL REFERENCES services (id) ON DELETE CASCADE,
    hostname      TEXT NOT NULL UNIQUE,
    upstream_host TEXT NOT NULL,
    upstream_port INTEGER NOT NULL,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (workspace_id, service_id)
) STRICT;

CREATE INDEX idx_preview_routes_port ON preview_routes (upstream_port);
CREATE INDEX idx_preview_routes_workspace ON preview_routes (workspace_id);
