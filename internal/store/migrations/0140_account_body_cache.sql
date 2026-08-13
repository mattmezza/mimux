-- 0140: per-account override for the global Settings → Syncing "keep bodies
-- downloaded" knob (how many of the newest inbox messages the warmer prefetches
-- and the body cache retains). NULL means "inherit the global value", same as
-- the other override columns added in 0120.
ALTER TABLE accounts ADD COLUMN body_cache INTEGER;
