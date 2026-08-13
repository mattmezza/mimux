-- 0120: per-account overrides for the global Settings → Syncing knobs (some
-- accounts get far more mail than others). NULL means "inherit the global
-- value", same as the empty-string convention used elsewhere on this table.
ALTER TABLE accounts ADD COLUMN sync_interval_min INTEGER;
ALTER TABLE accounts ADD COLUMN max_per_sync       INTEGER;
ALTER TABLE accounts ADD COLUMN sync_months        INTEGER;
