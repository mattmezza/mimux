-- 0110: reusable message templates (name + body) inserted from the compose
-- panel. Deliberately format-agnostic: one plain body, prepended into whichever
-- editor (plain/markdown/html) is active — same as an AI draft.
CREATE TABLE templates (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL DEFAULT '',
    body       TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL DEFAULT '',
    updated_at TEXT    NOT NULL DEFAULT ''
);
