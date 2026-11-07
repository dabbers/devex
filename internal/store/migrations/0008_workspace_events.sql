-- Workspace activity on the audit trail.
--
-- An event could already be scoped to a repo, a task or a fork. A workspace is
-- none of those: it outlives any task, and a workspace somebody opened by hand
-- belongs to no fork at all. Without a column of its own, "what happened in
-- this workspace" could only be answered by reading every event and guessing
-- from the message, which is not an audit trail.
ALTER TABLE events ADD COLUMN workspace_id TEXT;

CREATE INDEX idx_events_workspace ON events (workspace_id, seq);
