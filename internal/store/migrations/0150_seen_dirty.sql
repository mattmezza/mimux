-- 0150: mark-read durability. A \Seen flip made locally (opening a message, the
-- read/unread toggle, a filter rule) is written to messages.is_read immediately
-- and pushed to IMAP in the background. That push can fail, time out, or still
-- be queued when the process restarts — and until 0150 nothing recorded that it
-- was still owed, so the flag never landed and the next sync happily overwrote
-- is_read with the server's stale "unread".
--
-- seen_dirty = 1 means "this row's \Seen state has not been confirmed on the
-- server yet": the account worker re-pushes it every sync cycle, and both sync
-- write paths leave is_read alone while it is set.
ALTER TABLE messages ADD COLUMN seen_dirty INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_messages_seen_dirty ON messages(account) WHERE seen_dirty = 1;
