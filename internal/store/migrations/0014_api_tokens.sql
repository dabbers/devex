-- Tokens that authenticate a request without an identity provider.
--
-- The control plane's own authentication is the provider's, and that is right
-- for a person with a browser. It is useless for everything else: a terminal,
-- a script, a cron job, this platform's own command line tool. A provider's
-- session token lasts about a minute by design, which is correct for a browser
-- that refreshes it silently and unusable for anything that does not.
--
-- So a token here names a dabberz user directly rather than standing for a
-- provider identity. What keeps that from being a second, weaker front door is
-- where one can be minted: only by a process that can already read this
-- database and the master key, which is to say by root on the host. Root can
-- read the vault and the whole audit trail anyway, so this grants nothing that
-- was not already granted -- it just makes it usable without a browser.
--
-- The token itself is never stored. The hash is enough to check one, and a
-- stolen backup of this table is then worth nothing on its own.
CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- What it is for, in the words of whoever made it. Shown when listing, so
    -- that revoking the right one does not require guessing.
    name         TEXT NOT NULL,
    -- SHA-256 of the presented token, hex. Not a password hash on purpose:
    -- the token is 256 bits of randomness rather than something a person
    -- chose, so there is nothing to brute force and nothing a slow hash buys.
    hash         TEXT NOT NULL UNIQUE,
    created_at   TEXT NOT NULL,
    -- Updated on use, so an unused token can be told from a live one before
    -- deciding whether revoking it will break something.
    last_used_at TEXT
) STRICT;

CREATE INDEX idx_api_tokens_user ON api_tokens (user_id);
