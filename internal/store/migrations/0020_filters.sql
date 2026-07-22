-- 0020: filter rules (auto-forward/move/label/etc on incoming mail).
-- Conditions/actions are stored as JSON arrays rather than child tables:
-- there is no need to query into them, only to load a whole rule at a time.
CREATE TABLE filter_rules (
    id INTEGER PRIMARY KEY,
    account TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,
    position INTEGER NOT NULL DEFAULT 0,
    enabled INTEGER NOT NULL DEFAULT 1,
    conditions TEXT NOT NULL DEFAULT '[]',
    actions TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_filter_rules_account_position ON filter_rules(account, position);
