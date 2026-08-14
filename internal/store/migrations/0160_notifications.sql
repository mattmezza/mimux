-- 0160: notifications (Settings → Notifications).
--
-- push_subs is one row per browser-per-device Web Push subscription. The
-- endpoint is a URL at the browser vendor's push service (Apple/Google/
-- Mozilla) that the vendor minted for that install; a 404 or 410 from it means
-- the subscription is dead (site data cleared, PWA reinstalled, vendor rotated
-- it) and the row is deleted — see mail.Manager.notifyPush.
--
-- push_keys holds this instance's VAPID key pair, self-generated on first use
-- so nothing has to be configured by hand. It is deliberately NOT in
-- app_settings: store.Export dumps every app_settings row into the (cleartext)
-- backup file, and the VAPID private key is the one credential that lets a
-- holder push arbitrary notifications to the user's devices. A separate table
-- keeps it out of the backup by construction rather than by a filter a future
-- refactor could quietly drop. Subscriptions stay out for the same reason —
-- and they are worthless elsewhere anyway, being bound to this key pair.
CREATE TABLE push_subs (
    endpoint TEXT PRIMARY KEY,
    p256dh TEXT NOT NULL,
    auth TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE push_keys (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    public TEXT NOT NULL,
    private TEXT NOT NULL
);
