-- 0100: per-account signatures with three format variants (plain/html/markdown)
-- and an apply mode (every message vs first message only), plus a link from a
-- sender identity (bare email address) to a single signature. No link = no
-- signature (the default).
CREATE TABLE signatures (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    account    TEXT    NOT NULL DEFAULT '',   -- owning account name (grouping/label)
    name       TEXT    NOT NULL DEFAULT '',
    text_plain TEXT    NOT NULL DEFAULT '',
    html       TEXT    NOT NULL DEFAULT '',
    markdown   TEXT    NOT NULL DEFAULT '',
    apply_mode TEXT    NOT NULL DEFAULT 'always', -- always | first
    created_at TEXT    NOT NULL DEFAULT '',
    updated_at TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE identity_signatures (
    address      TEXT    PRIMARY KEY,           -- lowercased bare email of the identity
    signature_id INTEGER NOT NULL REFERENCES signatures(id) ON DELETE CASCADE
);
