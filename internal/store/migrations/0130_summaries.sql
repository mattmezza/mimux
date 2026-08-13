-- 0130: AI summary cache.
CREATE TABLE summaries (
    cache_key TEXT PRIMARY KEY,
    summary TEXT NOT NULL,
    truncated INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
