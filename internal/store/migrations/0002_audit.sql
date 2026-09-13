-- Make the activity feed an audit trail that works across every repo at once.
--
-- Without a repo on each row the feed can only be read per task or per fork,
-- so there is no single place to see everything in flight. Without an actor
-- there is no way to tell what the user did from what an agent did, which is
-- the question an audit trail exists to answer.
--
-- Rows written before this migration keep a NULL repo and an empty actor:
-- they are still readable, just not attributable.

ALTER TABLE events ADD COLUMN repo_id TEXT;
ALTER TABLE events ADD COLUMN actor TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_events_repo ON events (repo_id, seq);
CREATE INDEX idx_events_actor ON events (actor, seq);
CREATE INDEX idx_events_type ON events (type, seq);
