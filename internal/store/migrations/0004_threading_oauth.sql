-- 0004: phase 4 — Gmail thread id + labels on messages, and OAuth2 token store.
ALTER TABLE messages ADD COLUMN gm_thrid TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN labels   TEXT NOT NULL DEFAULT ''; -- space-joined Gmail labels

CREATE TABLE oauth_tokens (
    account       TEXT PRIMARY KEY,
    access_token  TEXT NOT NULL DEFAULT '',
    refresh_token TEXT NOT NULL DEFAULT '',
    expiry        TEXT NOT NULL DEFAULT '', -- RFC3339, empty = no expiry known
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
