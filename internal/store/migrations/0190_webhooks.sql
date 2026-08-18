-- 0190: outbound webhooks — endpoints and their delivery log.
--
-- The endpoints are configured in Settings → API by the human; the deliveries
-- are made by the pro layer (pro/webhooks.go). The free build fills this table
-- and never drains it, which the UI says out loud.
--
-- secret is stored in cleartext, unlike api_tokens.hash: it is an outbound HMAC
-- key the server itself must be able to read on every attempt, so there is no
-- one-way form of it that would still work. Same trust level as the IMAP
-- passwords and OAuth tokens already in this database.
--
-- events is a space-separated subset of store.WebhookEvents, the same encoding
-- api_tokens.scopes uses.
--
-- Deliberately NOT in the portable backup (see store.Export): an endpoint plus
-- its signing secret is install-local, like the API tokens in 0180.
CREATE TABLE webhook_endpoints (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    url              TEXT    NOT NULL,
    secret           TEXT    NOT NULL,
    events           TEXT    NOT NULL DEFAULT '',
    active           INTEGER NOT NULL DEFAULT 1,
    -- Set when the delivery engine gave up on this endpoint (a delivery
    -- exhausted the retry ladder). NULL means "never auto-disabled"; it is
    -- cleared when the user re-activates the endpoint by hand.
    auto_disabled_at TEXT,
    created_at       TEXT    NOT NULL DEFAULT ''
);

-- One row per (event, endpoint) pair, created when the event fires and updated
-- in place by each attempt. Pruned to the last 100 per endpoint on insert: this
-- is a log to look at after something broke, not an archive.
--
-- delivery_id is the identifier the receiver sees (X-Mimux-Delivery-Id) and it
-- stays the same across retries and replays, so a receiver can deduplicate —
-- delivery is at-least-once.
--
-- payload is the exact JSON body that gets POSTed, byte for byte, because the
-- signature is computed over it on every attempt.
CREATE TABLE webhook_deliveries (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    endpoint_id      INTEGER NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
    event_type       TEXT    NOT NULL,
    delivery_id      TEXT    NOT NULL,
    payload          TEXT    NOT NULL,
    -- pending (queued) | failed (attempted, retry scheduled) | ok | dead
    status           TEXT    NOT NULL DEFAULT 'pending',
    attempts         INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  TEXT    NOT NULL DEFAULT '',
    last_status_code INTEGER NOT NULL DEFAULT 0,
    last_error       TEXT    NOT NULL DEFAULT '',
    created_at       TEXT    NOT NULL DEFAULT '',
    delivered_at     TEXT
);

-- The drain query: due rows, oldest first.
CREATE INDEX idx_webhook_deliveries_due ON webhook_deliveries(status, next_attempt_at);
-- The log query, and the prune.
CREATE INDEX idx_webhook_deliveries_endpoint ON webhook_deliveries(endpoint_id, id);
