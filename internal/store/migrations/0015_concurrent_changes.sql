-- Several changes open against one installation at once.
--
-- Each starts from whatever production was when it was opened, and the
-- customer puts them live in the order they choose. A change that is not the
-- first to go live finds production has moved on since it started; it is
-- brought up to date before it follows, and when the two edit the same lines
-- the files are recorded here and the change waits for its own agent.
ALTER TABLE changes ADD COLUMN base_revision TEXT NOT NULL DEFAULT '';
-- A JSON list of paths, read by a person rather than queried.
ALTER TABLE changes ADD COLUMN conflicts TEXT NOT NULL DEFAULT '[]';
