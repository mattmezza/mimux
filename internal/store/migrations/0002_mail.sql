-- 0002: mail storage — folders, messages, sender preferences.

CREATE TABLE folders (
    id            INTEGER PRIMARY KEY,
    account       TEXT    NOT NULL,
    name          TEXT    NOT NULL,          -- IMAP mailbox name, e.g. "INBOX", "[Gmail]/Sent Mail"
    special_use   TEXT    NOT NULL DEFAULT '', -- inbox|sent|drafts|trash|spam|archive|''
    uidvalidity   INTEGER NOT NULL DEFAULT 0,
    highestmodseq INTEGER NOT NULL DEFAULT 0,
    unread_count  INTEGER NOT NULL DEFAULT 0,
    sort          INTEGER NOT NULL DEFAULT 0, -- display order
    UNIQUE(account, name)
);

CREATE TABLE messages (
    id            INTEGER PRIMARY KEY,
    account       TEXT    NOT NULL,
    folder_id     INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    uid           INTEGER NOT NULL,
    message_id    TEXT    NOT NULL DEFAULT '',
    in_reply_to   TEXT    NOT NULL DEFAULT '',
    refs          TEXT    NOT NULL DEFAULT '',
    from_name     TEXT    NOT NULL DEFAULT '',
    from_address  TEXT    NOT NULL DEFAULT '',
    to_addresses  TEXT    NOT NULL DEFAULT '',
    cc_addresses  TEXT    NOT NULL DEFAULT '',
    subject       TEXT    NOT NULL DEFAULT '',
    date          TEXT    NOT NULL DEFAULT '',
    size          INTEGER NOT NULL DEFAULT 0,
    is_read       INTEGER NOT NULL DEFAULT 0,
    is_starred    INTEGER NOT NULL DEFAULT 0,
    has_attachment INTEGER NOT NULL DEFAULT 0,
    snippet       TEXT    NOT NULL DEFAULT '',
    UNIQUE(folder_id, uid)
);

-- Indexes serve the message list and later search.
CREATE INDEX idx_messages_from    ON messages(from_address);
CREATE INDEX idx_messages_subject ON messages(subject);
CREATE INDEX idx_messages_date    ON messages(date);
CREATE INDEX idx_messages_read    ON messages(is_read);
CREATE INDEX idx_messages_starred ON messages(is_starred);
CREATE INDEX idx_messages_folder  ON messages(folder_id, date);

CREATE TABLE sender_prefs (
    address        TEXT PRIMARY KEY,
    allow_external INTEGER NOT NULL DEFAULT 0
);
