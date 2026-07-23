-- 0070: email accounts move from config.toml into the DB, managed from the
-- Settings GUI. Aliases are a JSON array (no need to query into them). Folders
-- and messages are keyed by account name (text), so re-adding an account with
-- the same name naturally reattaches its already-synced mail.
CREATE TABLE accounts (
    name                 TEXT    PRIMARY KEY,        -- identity/key + badge label
    sender_name          TEXT    NOT NULL DEFAULT '',
    provider             TEXT    NOT NULL DEFAULT '',
    email                TEXT    NOT NULL DEFAULT '',
    auth                 TEXT    NOT NULL DEFAULT 'password', -- password | oauth2
    password             TEXT    NOT NULL DEFAULT '',
    oauth2_client_id     TEXT    NOT NULL DEFAULT '',
    oauth2_client_secret TEXT    NOT NULL DEFAULT '',
    imap_host            TEXT    NOT NULL DEFAULT '',
    imap_port            INTEGER NOT NULL DEFAULT 0,
    smtp_host            TEXT    NOT NULL DEFAULT '',
    smtp_port            INTEGER NOT NULL DEFAULT 0,
    aliases              TEXT    NOT NULL DEFAULT '[]', -- JSON array of {name,email}
    position             INTEGER NOT NULL DEFAULT 0
);
