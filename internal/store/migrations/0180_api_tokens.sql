-- 0180: personal access tokens for the machine-facing API (Settings → API).
--
-- Only the argon2id hash of a token is kept; the secret itself is shown to the
-- user once, at creation, and is unrecoverable afterwards — the same deal as
-- the login password in users.password_hash.
--
-- scopes is a space-separated subset of store.APIScopes. The three timestamps
-- are nullable rather than blank-defaulted because "never used", "never
-- expires" and "not revoked" are genuinely absent values, and the middleware's
-- active-token query says so directly (revoked_at IS NULL).
--
-- These rows DO travel in the portable backup (see store.Export, v3): the hash
-- is the credential as far as this database is concerned, so restoring it is
-- what lets a rebuilt install keep honouring the tokens already handed out.
CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    label        TEXT    NOT NULL DEFAULT '',
    hash         TEXT    NOT NULL,
    scopes       TEXT    NOT NULL DEFAULT 'mail:read',
    created_at   TEXT    NOT NULL DEFAULT '',
    last_used_at TEXT,
    expires_at   TEXT,
    revoked_at   TEXT
);
