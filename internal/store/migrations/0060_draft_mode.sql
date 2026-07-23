-- 0060: remember which compose editor authored a draft (plain|html|markdown)
-- so reopening restores the right editor and the raw body renders faithfully.
ALTER TABLE drafts ADD COLUMN mode TEXT NOT NULL DEFAULT 'plain';
