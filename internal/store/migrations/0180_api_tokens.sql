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
-- Deliberately NOT in the portable backup (see store.Export): a token is an
-- install-local credential, like the VAPID key pair in 0160.
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
