-- An external identity for each user.
--
-- A user used to be identified by email alone, which was workable while there
-- was one of them named in a configuration file. With an identity provider,
-- email is the wrong key: a person can change theirs, and a provider can
-- reassign a freed address to somebody else. The provider's subject claim is
-- stable for the life of the account, so that becomes the key and email
-- becomes a display and attribution detail.
--
-- Nullable, because the single-user and development paths have no provider,
-- and unique so two provider accounts cannot collapse into one dabberz user.
ALTER TABLE users ADD COLUMN external_id TEXT;

CREATE UNIQUE INDEX idx_users_external_id ON users (external_id) WHERE external_id IS NOT NULL;
