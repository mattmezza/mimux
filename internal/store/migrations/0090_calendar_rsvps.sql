-- 0090: calendar RSVPs — the local record of the user's response to an
-- iCalendar (iTIP) invite, so the reading-pane card reflects the chosen state
-- on later views and after re-sync. Keyed by (account, uid) so it survives the
-- message row being re-fetched; sequence lets an updated invitation mark a
-- prior response stale. See store/calendar.go + server/calendar.go.
CREATE TABLE calendar_rsvps (
    account    TEXT    NOT NULL,
    uid        TEXT    NOT NULL,
    partstat   TEXT    NOT NULL,              -- ACCEPTED | TENTATIVE | DECLINED
    sequence   INTEGER NOT NULL DEFAULT 0,    -- the event SEQUENCE we replied to
    updated_at TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (account, uid)
);
