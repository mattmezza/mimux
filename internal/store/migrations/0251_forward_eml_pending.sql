-- Preserve an EML forward across a failed source fetch or draft save.
ALTER TABLE drafts ADD COLUMN forward_eml_id INTEGER NOT NULL DEFAULT 0;
