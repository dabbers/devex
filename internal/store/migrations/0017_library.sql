-- The account's library: files and notes agents and people keep, for the whole
-- account or for one app it runs.
--
-- The scope is '' for the account and an installation's ID for that app's own
-- shelf -- never NULL, because a NULL is distinct from every other NULL and
-- the uniqueness of a path within its scope would quietly stop holding.
--
-- Bytes are not here. They are kept by content hash on disk, written by the
-- platform after it decided the file is safe to keep; see internal/library.
CREATE TABLE library_files (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    scope        TEXT NOT NULL DEFAULT '',
    path         TEXT NOT NULL,
    kind         TEXT NOT NULL,
    content_type TEXT NOT NULL,
    size         INTEGER NOT NULL,
    sha256       TEXT NOT NULL,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    UNIQUE (user_id, scope, path)
) STRICT;

CREATE INDEX library_files_sha ON library_files (sha256);

-- What an installation's owner chose for the settings its app declares.
--
-- Only the values. Which settings exist is the app's to say, in its own
-- code, and changes when the app does; a value for a setting the app no
-- longer has is kept rather than dropped, so taking a change back does not
-- lose what was chosen for it.
CREATE TABLE installation_settings (
    installation_id TEXT PRIMARY KEY REFERENCES installations (id) ON DELETE CASCADE,
    vals            TEXT NOT NULL DEFAULT '{}',
    updated_by      TEXT NOT NULL DEFAULT '',
    updated_at      TEXT NOT NULL
) STRICT;
