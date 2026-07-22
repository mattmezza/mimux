-- 0050: cache parsed message bodies (attachments excluded) so subsequent opens
-- read from SQLite instead of re-fetching the raw RFC822 from the IMAP server.
-- ON DELETE CASCADE (FKs are on) drops the cache row when the message is
-- expunged/moved out.
CREATE TABLE message_bodies (
    message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    body       BLOB    NOT NULL,
    fetched_at INTEGER NOT NULL
);
