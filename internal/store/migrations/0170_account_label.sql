-- Display label for an account, shown wherever the UI names it (list badge,
-- sidebar, notifications). The name column stays the immutable key that
-- folders, messages, tokens, filters and colors are keyed by; this is the part
-- the user can change. Blank means "use the name".
ALTER TABLE accounts ADD COLUMN label TEXT NOT NULL DEFAULT '';
