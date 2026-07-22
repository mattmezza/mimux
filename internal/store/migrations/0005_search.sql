-- 0005: phase 5 — full-text search index, server-search cache, history, saved.

-- Contentless FTS5 index keyed to messages.id. Columns are computed/concatenated
-- (from_name+from_address, to+cc), so they don't map 1:1 to a content table —
-- a contentless table with contentless_delete is the right fit: triggers supply
-- every token, deletes/updates are the 'delete' special-insert with old values.
CREATE VIRTUAL TABLE messages_fts USING fts5(
    subject, snippet, from_addr, to_addr,
    content='', contentless_delete=1
);

CREATE TRIGGER messages_fts_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, subject, snippet, from_addr, to_addr)
    VALUES (new.id, new.subject, new.snippet,
            new.from_name || ' ' || new.from_address,
            new.to_addresses || ' ' || new.cc_addresses);
END;

CREATE TRIGGER messages_fts_ad AFTER DELETE ON messages BEGIN
    DELETE FROM messages_fts WHERE rowid = old.id;
END;

CREATE TRIGGER messages_fts_au AFTER UPDATE ON messages BEGIN
    DELETE FROM messages_fts WHERE rowid = old.id;
    INSERT INTO messages_fts(rowid, subject, snippet, from_addr, to_addr)
    VALUES (new.id, new.subject, new.snippet,
            new.from_name || ' ' || new.from_address,
            new.to_addresses || ' ' || new.cc_addresses);
END;

-- Backfill existing rows.
INSERT INTO messages_fts(rowid, subject, snippet, from_addr, to_addr)
SELECT id, subject, snippet, from_name || ' ' || from_address,
       to_addresses || ' ' || cc_addresses
FROM messages;

CREATE TABLE search_cache (
    query_hash  TEXT PRIMARY KEY,
    message_ids TEXT NOT NULL,               -- JSON array of message ids
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE search_history (
    id         INTEGER PRIMARY KEY,
    query      TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE saved_searches (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    query      TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
