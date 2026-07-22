-- 0003: local compose drafts (auto-saved while composing). Local-only: no
-- IMAP Drafts-folder sync in this phase (see internal/store/drafts.go).
CREATE TABLE drafts (
    id            INTEGER PRIMARY KEY,
    account       TEXT NOT NULL DEFAULT '',
    to_addresses  TEXT NOT NULL DEFAULT '',
    cc_addresses  TEXT NOT NULL DEFAULT '',
    bcc_addresses TEXT NOT NULL DEFAULT '',
    subject       TEXT NOT NULL DEFAULT '',
    body          TEXT NOT NULL DEFAULT '',
    in_reply_to   TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL DEFAULT 'new', -- new|reply|reply_all|forward
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
