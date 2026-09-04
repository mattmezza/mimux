-- 0250: remember which attachments from an original message a forward carries.
-- Bytes stay on the source IMAP message until Send is pressed; only safe
-- metadata and MIME part paths live with the local draft.
ALTER TABLE drafts ADD COLUMN forward_source_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE drafts ADD COLUMN forward_attachments TEXT NOT NULL DEFAULT '';
ALTER TABLE drafts ADD COLUMN forward_attachments_initialized INTEGER NOT NULL DEFAULT 0;
