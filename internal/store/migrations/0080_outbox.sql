-- 0080: outbox — messages queued for delayed ("undo send") or scheduled
-- delivery. Holds the full compose payload (incl. attachments as a base64 JSON
-- blob) so the scheduler can build + send after a restart. See store/outbox.go.
CREATE TABLE outbox (
    id            INTEGER PRIMARY KEY,
    account       TEXT NOT NULL DEFAULT '',
    from_addr     TEXT NOT NULL DEFAULT '',
    to_addresses  TEXT NOT NULL DEFAULT '',
    cc_addresses  TEXT NOT NULL DEFAULT '',
    bcc_addresses TEXT NOT NULL DEFAULT '',
    subject       TEXT NOT NULL DEFAULT '',
    body          TEXT NOT NULL DEFAULT '',
    mode          TEXT NOT NULL DEFAULT 'plain',
    in_reply_to   TEXT NOT NULL DEFAULT '',
    refs          TEXT NOT NULL DEFAULT '',
    attachments   TEXT NOT NULL DEFAULT '',  -- JSON [{Filename,ContentType,Data(base64)}]
    send_at       TEXT NOT NULL,             -- RFC3339 UTC
    status        TEXT NOT NULL DEFAULT 'queued', -- queued|sending|sent|failed|cancelled
    error         TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX outbox_due ON outbox (status, send_at);
