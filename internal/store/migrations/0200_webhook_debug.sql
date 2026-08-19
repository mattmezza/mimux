-- 0200: what the webhook deliveries screen needs to debug a receiver, plus the
-- stamp that keeps failure mail from becoming its own outage.
--
-- response_body is the first bytes of what the receiver answered, capped by the
-- delivery engine before it ever reaches this column. The engine used to read
-- the reply only to discard it (so the connection could be reused); keeping a
-- slice of it is the difference between "HTTP 400" and "HTTP 400: unknown event
-- type". It is the receiver's own words about our request, not mail content.
--
-- duration_ms is how long the last attempt took, end to end. A receiver that
-- answers 200 in nine seconds is one timeout away from failing, and nothing
-- else in the log says so.
ALTER TABLE webhook_deliveries ADD COLUMN response_body TEXT NOT NULL DEFAULT '';
ALTER TABLE webhook_deliveries ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0;

-- When we last emailed about this endpoint failing. NULL = never. One column
-- per endpoint rather than one row per (user, day): this database has exactly
-- one user, and the endpoint is the thing that is broken.
ALTER TABLE webhook_endpoints ADD COLUMN failure_email_at TEXT;

-- The deliveries screen filters by status and by event type within one
-- endpoint, newest first.
CREATE INDEX idx_webhook_deliveries_filter ON webhook_deliveries(endpoint_id, status, event_type, id);
