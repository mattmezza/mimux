-- 0230: a draft remembers where it lives in the mailbox.
--
-- Drafts were local-only: saved to SQLite and nowhere else, so the half-written
-- reply you left on the laptop did not exist on the phone. The table is now a
-- write-through cache of the account's IMAP Drafts folder.
--
-- message_id is the draft's identity across revisions. Every save APPENDs a
-- fresh copy and expunges the previous one, so the UID changes constantly;
-- the Message-ID does not. It is what the no-UIDPLUS server is searched by,
-- and what the drafts page dedups a local row against its own synced copy on.
--
-- folder_id/uid point at the revision currently sitting on the server (0 =
-- never published, or published by a server that reported no APPENDUID).
--
-- imap_dirty = 1 means "this row's content has not been published yet": the
-- account worker retries it every sync cycle, exactly like seen_dirty (0150).
-- Existing rows are marked dirty so they drain through that same retry on the
-- first healthy cycle — no bulk push at boot.
ALTER TABLE drafts ADD COLUMN message_id TEXT NOT NULL DEFAULT '';
ALTER TABLE drafts ADD COLUMN folder_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE drafts ADD COLUMN uid INTEGER NOT NULL DEFAULT 0;
ALTER TABLE drafts ADD COLUMN imap_dirty INTEGER NOT NULL DEFAULT 0;
UPDATE drafts SET imap_dirty = 1;
CREATE INDEX idx_drafts_imap_dirty ON drafts(account) WHERE imap_dirty = 1;
