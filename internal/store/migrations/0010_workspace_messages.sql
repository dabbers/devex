-- A conversation attached to a workspace.
--
-- Work could only be asked for by writing a request, waiting for a plan,
-- approving it and watching forks run. That is the right shape for "do these
-- four things in parallel while I am away" and the wrong one for "the spacing
-- is off, fix it" -- which is most of what working on something actually is.
--
-- A workspace already has the machine, the branch, the services and the
-- preview. What it lacked was somewhere to say what you want.
CREATE TABLE workspace_messages (
    -- A monotonic sequence rather than a timestamp, for the same reason the
    -- event feed has one: identifiers and timestamps only order to
    -- millisecond precision, and two turns in quick succession can land in
    -- the same millisecond. A reply sorting before the prompt it answers
    -- reads as the agent replying before it was asked.
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    id           TEXT NOT NULL UNIQUE,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- "user" or "agent". System notes use "agent" with a state that says what
    -- happened, rather than a third role nothing else understands.
    role         TEXT NOT NULL,
    content      TEXT NOT NULL DEFAULT '',
    -- "running" while the agent is working, then "done" or "failed". A user
    -- message is written "done": it is complete the moment it is said.
    state        TEXT NOT NULL,
    state_reason TEXT NOT NULL DEFAULT '',
    usage_json   TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

CREATE INDEX idx_workspace_messages ON workspace_messages (workspace_id, seq);

-- The workspace's own conversation with the agent, the same shape a fork
-- carries. Without it every message would start from an empty context, which
-- is exactly the thing that makes a chat useless.
ALTER TABLE workspaces ADD COLUMN agent_session_json TEXT;
