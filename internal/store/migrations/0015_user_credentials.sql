-- Credentials that belong to a person rather than to a repo.
--
-- A repo secret is inherited by every fork and injected into every service the
-- repo runs, which is right for a database password and wrong for the
-- subscription that pays for the agent: that is the customer's, not their
-- app's, and it should reach the agent process and nothing else. So it is kept
-- apart, keyed by the user, and sealed with the same master key and cipher as
-- repo secrets.
--
-- The kind names which credential this is. Only one is used today -- the
-- agent's Claude token -- but the shape is the same for any other per-person
-- credential, and a second table per kind would be the same table again.
CREATE TABLE user_credentials (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    nonce      BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (user_id, kind)
) STRICT;
