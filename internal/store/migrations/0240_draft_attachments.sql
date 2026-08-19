-- 0240: a draft keeps its attachments.
--
-- Until now the files in a compose window were request-scoped: they rode along
-- with the send and nothing else. Saving a draft dropped them, reopening one
-- showed none, and the copy published to the IMAP Drafts folder (0230) was
-- always attachment-free — so the half-written mail that reached your phone was
-- missing the very thing you were writing about.
--
-- One row per file, bytes and all. Drafts are few and their attachments are
-- capped at the same 25MB total the send path enforces (maxAttachTotal), so a
-- BLOB in SQLite is the whole story — the same shape message_bodies (0050)
-- already uses for cached bodies.
--
-- ON DELETE CASCADE (FKs are on, see store.Open): sending or discarding a draft
-- takes its files with it, and there is no second place to clean up.
CREATE TABLE draft_attachments (
    id           INTEGER PRIMARY KEY,
    draft_id     INTEGER NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
    filename     TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL DEFAULT '',
    content      BLOB NOT NULL
);
CREATE INDEX idx_draft_attachments_draft ON draft_attachments(draft_id);
