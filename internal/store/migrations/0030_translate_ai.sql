-- 0030: translation cache.
CREATE TABLE translations (
    cache_key TEXT PRIMARY KEY,
    translated_text TEXT NOT NULL,
    detected_lang TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
