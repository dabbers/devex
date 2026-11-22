-- Waitlist for dabberz.com. Apply with:
--   npx wrangler d1 execute dabberz-waitlist --remote --file functions/api/schema.sql
--
-- email is the primary key, so INSERT OR IGNORE makes a repeat signup a no-op.
CREATE TABLE IF NOT EXISTS waitlist (
  email      TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  source     TEXT
);

CREATE INDEX IF NOT EXISTS waitlist_created_at ON waitlist (created_at);
