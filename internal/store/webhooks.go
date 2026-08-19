// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"database/sql"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"
)

// WebhookEvents are the event types an endpoint can subscribe to, in display
// order. An endpoint's events column is a space-separated subset of these IDs;
// anything else is dropped on the way in (see ValidWebhookEvents), so a typo
// cannot be stored and later matched against.
//
// `ping` is not here on purpose: it is only ever sent by the explicit "send a
// test delivery" action, which ignores the subscription list — subscribing to a
// test event would be a way to never receive one.
var WebhookEvents = []struct{ ID, Label string }{
	{"message.received", "A message arrived in an inbox"},
	{"message.sent", "A message was sent"},
	{"sync.error", "An account failed to sync"},
	{"search.completed", "A deep-search job finished"},
}

// Delivery states. A row is pending until the first attempt, failed while
// retries remain, then ok or dead.
const (
	WebhookPending = "pending"
	WebhookFailed  = "failed"
	WebhookOK      = "ok"
	WebhookDead    = "dead"
)

// webhookDeliveryKeep is how many delivery rows are kept per endpoint. This is
// a log you read after something broke, not an archive.
const webhookDeliveryKeep = 100

// WebhookEndpoint is one subscriber: a URL, the events it wants, and the secret
// its signatures are computed with.
//
// Secret is stored (and read back) in cleartext — see migration 0190. It is an
// outbound HMAC key, so a one-way hash of it would be useless to the sender.
type WebhookEndpoint struct {
	ID             int64
	URL            string
	Secret         string
	Events         string    // space-separated, subset of WebhookEvents
	Active         bool      // false = paused (by hand, or auto-disabled)
	AutoDisabledAt time.Time // zero = never auto-disabled
	FailureEmailAt time.Time // zero = we never emailed about this one failing
	CreatedAt      time.Time
}

// EventList is the endpoint's events as a slice, for rendering and JSON.
func (e WebhookEndpoint) EventList() []string { return strings.Fields(e.Events) }

// Wants reports whether this endpoint subscribed to an event type.
func (e WebhookEndpoint) Wants(event string) bool {
	for _, s := range strings.Fields(e.Events) {
		if s == event {
			return true
		}
	}
	return false
}

// AutoDisabled reports whether the delivery engine turned this endpoint off.
func (e WebhookEndpoint) AutoDisabled() bool { return !e.AutoDisabledAt.IsZero() }

// ValidWebhookEvents filters requested event types down to the known ones,
// deduplicated and in WebhookEvents order, as the stored space-separated
// string. Unlike token scopes there is no fallback: an endpoint subscribed to
// nothing is a legal (if pointless) state, and inventing a subscription the
// user did not ask for would send them mail metadata they never requested.
func ValidWebhookEvents(want []string) string {
	order := map[string]int{}
	for i, e := range WebhookEvents {
		order[e.ID] = i
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range want {
		w = strings.TrimSpace(w)
		if _, ok := order[w]; !ok || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return strings.Join(out, " ")
}

// normalizeWebhookURL rejects anything the delivery engine must not POST to.
// Enforced here rather than in each caller, so the HTML form and the JSON API
// cannot disagree about it: a file:// or javascript: "endpoint" would either
// fail forever or be an attack surface, and neither belongs in the table.
func normalizeWebhookURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("webhook: url must be an absolute http(s) URL")
	}
	return raw, nil
}

const webhookEndpointCols = `id, url, secret, events, active, auto_disabled_at, failure_email_at, created_at`

func scanWebhookEndpoint(sc interface{ Scan(...any) error }) (WebhookEndpoint, error) {
	var e WebhookEndpoint
	var created string
	var disabled, emailed sql.NullString
	if err := sc.Scan(&e.ID, &e.URL, &e.Secret, &e.Events, &e.Active, &disabled, &emailed, &created); err != nil {
		return e, err
	}
	e.CreatedAt, _ = time.Parse(time.RFC3339, created)
	e.AutoDisabledAt = parseNullTime(disabled)
	e.FailureEmailAt = parseNullTime(emailed)
	return e, nil
}

// CreateWebhookEndpoint stores a new endpoint, setting ep.ID and ep.CreatedAt.
// The secret is the caller's — both callers generate one with auth.NewToken.
func (s *Store) CreateWebhookEndpoint(ep *WebhookEndpoint) error {
	u, err := normalizeWebhookURL(ep.URL)
	if err != nil {
		return err
	}
	if ep.Secret == "" {
		return errors.New("webhook: secret required")
	}
	ep.URL = u
	ep.Events = ValidWebhookEvents(strings.Fields(ep.Events))
	ep.CreatedAt = time.Now().UTC().Truncate(time.Second)
	res, err := s.DB.Exec(`INSERT INTO webhook_endpoints (url, secret, events, active, created_at)
		VALUES (?,?,?,?,?)`, ep.URL, ep.Secret, ep.Events, b2i(ep.Active), ep.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return err
	}
	ep.ID, err = res.LastInsertId()
	return err
}

// ListWebhookEndpoints returns every endpoint, newest first. Small by nature
// (single-user install), so the delivery engine simply lists and filters in Go
// rather than asking for a per-event subset.
func (s *Store) ListWebhookEndpoints() ([]WebhookEndpoint, error) {
	rows, err := s.DB.Query(`SELECT ` + webhookEndpointCols + ` FROM webhook_endpoints ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []WebhookEndpoint
	for rows.Next() {
		ep, err := scanWebhookEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// WebhookEndpointByID returns one endpoint, or nil if absent.
func (s *Store) WebhookEndpointByID(id int64) (*WebhookEndpoint, error) {
	ep, err := scanWebhookEndpoint(s.DB.QueryRow(`SELECT `+webhookEndpointCols+` FROM webhook_endpoints WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ep, nil
}

// UpdateWebhookEndpoint writes back the mutable fields (url, events, active,
// auto-disabled state). The secret has its own setter — see SetWebhookSecret —
// so that rotating it is an explicit act and not something a stale struct can
// do by accident on its way through a form handler.
func (s *Store) UpdateWebhookEndpoint(ep *WebhookEndpoint) error {
	u, err := normalizeWebhookURL(ep.URL)
	if err != nil {
		return err
	}
	ep.URL = u
	ep.Events = ValidWebhookEvents(strings.Fields(ep.Events))
	_, err = s.DB.Exec(`UPDATE webhook_endpoints SET url = ?, events = ?, active = ?, auto_disabled_at = ? WHERE id = ?`,
		ep.URL, ep.Events, b2i(ep.Active), nullTime(ep.AutoDisabledAt), ep.ID)
	return err
}

// AutoDisableWebhookEndpoint is what the delivery engine calls when it gives up
// on an endpoint: it stops firing, and the UI says why. Re-activating it by
// hand clears the stamp.
func (s *Store) AutoDisableWebhookEndpoint(id int64, at time.Time) error {
	_, err := s.DB.Exec(`UPDATE webhook_endpoints SET active = 0, auto_disabled_at = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339), id)
	return err
}

// SetWebhookSecret rotates an endpoint's signing key. Every attempt made after
// this — including a replay of an old delivery — signs with the new secret, so
// the receiver has to be updated at the same time.
func (s *Store) SetWebhookSecret(id int64, secret string) error {
	if strings.TrimSpace(secret) == "" {
		return errors.New("webhook: secret required")
	}
	_, err := s.DB.Exec(`UPDATE webhook_endpoints SET secret = ? WHERE id = ?`, secret, id)
	return err
}

// MarkWebhookFailureEmail stamps when we last emailed about this endpoint
// failing. The stamp *is* the dedupe: see WebhookEndpoint.FailureEmailDue.
func (s *Store) MarkWebhookFailureEmail(id int64, at time.Time) error {
	_, err := s.DB.Exec(`UPDATE webhook_endpoints SET failure_email_at = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339), id)
	return err
}

// FailureEmailDue reports whether a failure notification may be sent now: at
// most one per endpoint per day, counted from the last one actually sent.
//
// Per endpoint rather than per user because the endpoint is the thing that
// broke and the row to hang the stamp on already exists — with one endpoint
// (the common case) the two are the same rule anyway.
func (e WebhookEndpoint) FailureEmailDue(now time.Time) bool {
	return now.Sub(e.FailureEmailAt) >= 24*time.Hour
}

// DeleteWebhookEndpoint removes an endpoint; its deliveries go with it (ON
// DELETE CASCADE).
func (s *Store) DeleteWebhookEndpoint(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM webhook_endpoints WHERE id = ?`, id)
	return err
}

// --- deliveries ---

// WebhookDelivery is one event queued for one endpoint, plus the outcome of the
// attempts made so far.
type WebhookDelivery struct {
	ID             int64
	EndpointID     int64
	EventType      string
	DeliveryID     string // stable across retries; the receiver deduplicates on it
	Payload        string // the exact JSON body, signed as-is on every attempt
	Status         string
	Attempts       int
	NextAttemptAt  time.Time
	LastStatusCode int
	LastError      string
	ResponseBody   string // first bytes of the receiver's reply to the last attempt
	DurationMS     int    // how long the last attempt took, end to end
	CreatedAt      time.Time
	DeliveredAt    time.Time // zero until a 2xx
}

// Succeeded reports whether this delivery is done and went through.
func (d WebhookDelivery) Succeeded() bool { return d.Status == WebhookOK }

const webhookDeliveryCols = `id, endpoint_id, event_type, delivery_id, payload, status, attempts,
	next_attempt_at, last_status_code, last_error, response_body, duration_ms, created_at, delivered_at`

func scanWebhookDelivery(sc interface{ Scan(...any) error }) (WebhookDelivery, error) {
	var d WebhookDelivery
	var next, created string
	var delivered sql.NullString
	if err := sc.Scan(&d.ID, &d.EndpointID, &d.EventType, &d.DeliveryID, &d.Payload, &d.Status,
		&d.Attempts, &next, &d.LastStatusCode, &d.LastError, &d.ResponseBody, &d.DurationMS,
		&created, &delivered); err != nil {
		return d, err
	}
	d.NextAttemptAt, _ = time.Parse(time.RFC3339, next)
	d.CreatedAt, _ = time.Parse(time.RFC3339, created)
	d.DeliveredAt = parseNullTime(delivered)
	return d, nil
}

// EnqueueWebhookDelivery queues one delivery and prunes the endpoint's log back
// to the last webhookDeliveryKeep rows. Pruning on insert (rather than on a
// timer) keeps the table bounded without another goroutine to own.
func (s *Store) EnqueueWebhookDelivery(d *WebhookDelivery) error {
	if d.Status == "" {
		d.Status = WebhookPending
	}
	d.CreatedAt = time.Now().UTC().Truncate(time.Second)
	if d.NextAttemptAt.IsZero() {
		d.NextAttemptAt = d.CreatedAt
	}
	res, err := s.DB.Exec(`INSERT INTO webhook_deliveries
		(endpoint_id, event_type, delivery_id, payload, status, next_attempt_at, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		d.EndpointID, d.EventType, d.DeliveryID, d.Payload, d.Status,
		d.NextAttemptAt.UTC().Format(time.RFC3339), d.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return err
	}
	if d.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	_, err = s.DB.Exec(`DELETE FROM webhook_deliveries WHERE endpoint_id = ? AND id NOT IN (
		SELECT id FROM webhook_deliveries WHERE endpoint_id = ? ORDER BY id DESC LIMIT ?)`,
		d.EndpointID, d.EndpointID, webhookDeliveryKeep)
	return err
}

// DueWebhookDeliveries returns the deliveries whose next attempt has come,
// oldest first.
func (s *Store) DueWebhookDeliveries(now time.Time, limit int) ([]WebhookDelivery, error) {
	rows, err := s.DB.Query(`SELECT `+webhookDeliveryCols+` FROM webhook_deliveries
		WHERE status IN (?,?) AND next_attempt_at <= ? ORDER BY id LIMIT ?`,
		WebhookPending, WebhookFailed, now.UTC().Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []WebhookDelivery
	for rows.Next() {
		d, err := scanWebhookDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListWebhookDeliveries returns an endpoint's log, newest first.
func (s *Store) ListWebhookDeliveries(endpointID int64, limit int) ([]WebhookDelivery, error) {
	rows, err := s.DB.Query(`SELECT `+webhookDeliveryCols+` FROM webhook_deliveries
		WHERE endpoint_id = ? ORDER BY id DESC LIMIT ?`, endpointID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []WebhookDelivery
	for rows.Next() {
		d, err := scanWebhookDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// WebhookDeliveryFilter narrows the deliveries screen. A blank field means "no
// filter on this one"; an unknown status or event simply matches nothing, which
// is what a hand-typed query string deserves.
type WebhookDeliveryFilter struct {
	Status string // one of the delivery states
	Event  string // an event type
	Limit  int    // page size (0 = 25)
	Offset int
}

// FilterWebhookDeliveries returns one page of an endpoint's log (newest first)
// plus the total number of rows matching the filter, for the pager.
func (s *Store) FilterWebhookDeliveries(endpointID int64, f WebhookDeliveryFilter) ([]WebhookDelivery, int, error) {
	where := `WHERE endpoint_id = ?`
	args := []any{endpointID}
	if f.Status != "" {
		where += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.Event != "" {
		where += ` AND event_type = ?`
		args = append(args, f.Event)
	}
	var total int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 25
	}
	rows, err := s.DB.Query(`SELECT `+webhookDeliveryCols+` FROM webhook_deliveries `+where+
		` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var out []WebhookDelivery
	for rows.Next() {
		d, err := scanWebhookDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}

// WebhookStats summarises an endpoint's delivery log — which is the last
// webhookDeliveryKeep deliveries, not all time. That is the honest window: the
// rows older than that were pruned, so a rate over "everything" would silently
// mean the same thing while sounding like it didn't.
type WebhookStats struct {
	Total      int
	OK         int
	Retrying   int       // attempted, still walking the retry ladder
	Dead       int       // gave up (or the receiver said 410)
	Pending    int       // queued, not yet attempted
	LastAt     time.Time // when the newest delivery was queued
	LastStatus string
	LastCode   int
}

// Failing is what the listing shows as the trouble count: everything that has
// failed at least once and hasn't been delivered.
func (s WebhookStats) Failing() int { return s.Retrying + s.Dead }

// Settled is the deliveries that reached a verdict: delivered, or given up on.
func (s WebhookStats) Settled() int { return s.OK + s.Dead }

// SuccessRate is the percentage of settled deliveries that were delivered.
// Meaningless (and unshown) when nothing has settled yet — see Settled.
func (s WebhookStats) SuccessRate() int {
	if s.Settled() == 0 {
		return 0
	}
	return s.OK * 100 / s.Settled()
}

// WebhookEndpointStats folds an endpoint's log into the numbers the listing
// shows: how much got through, what is broken now, and when it last tried.
func (s *Store) WebhookEndpointStats(endpointID int64) (WebhookStats, error) {
	var st WebhookStats
	rows, err := s.DB.Query(`SELECT status, COUNT(*) FROM webhook_deliveries
		WHERE endpoint_id = ? GROUP BY status`, endpointID)
	if err != nil {
		return st, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return st, err
		}
		st.Total += n
		switch status {
		case WebhookOK:
			st.OK = n
		case WebhookFailed:
			st.Retrying = n
		case WebhookDead:
			st.Dead = n
		case WebhookPending:
			st.Pending = n
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	var created string
	err = s.DB.QueryRow(`SELECT status, last_status_code, created_at FROM webhook_deliveries
		WHERE endpoint_id = ? ORDER BY id DESC LIMIT 1`, endpointID).
		Scan(&st.LastStatus, &st.LastCode, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.LastAt, _ = time.Parse(time.RFC3339, created)
	return st, nil
}

// WebhookDeliveryByID returns one delivery, or nil if absent.
func (s *Store) WebhookDeliveryByID(id int64) (*WebhookDelivery, error) {
	d, err := scanWebhookDelivery(s.DB.QueryRow(`SELECT `+webhookDeliveryCols+` FROM webhook_deliveries WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// SaveWebhookDelivery writes back the attempt outcome.
func (s *Store) SaveWebhookDelivery(d *WebhookDelivery) error {
	_, err := s.DB.Exec(`UPDATE webhook_deliveries SET status = ?, attempts = ?, next_attempt_at = ?,
		last_status_code = ?, last_error = ?, response_body = ?, duration_ms = ?, delivered_at = ?
		WHERE id = ?`,
		d.Status, d.Attempts, d.NextAttemptAt.UTC().Format(time.RFC3339),
		d.LastStatusCode, d.LastError, d.ResponseBody, d.DurationMS, nullTime(d.DeliveredAt), d.ID)
	return err
}

// ReplayWebhookDelivery re-queues a delivery for immediate re-sending, with a
// fresh retry budget and the same delivery_id — a receiver that already got it
// sees the same X-Mimux-Delivery-Id and can ignore the duplicate.
func (s *Store) ReplayWebhookDelivery(id int64, at time.Time) error {
	_, err := s.DB.Exec(`UPDATE webhook_deliveries
		SET status = ?, attempts = 0, next_attempt_at = ?, delivered_at = NULL WHERE id = ?`,
		WebhookPending, at.UTC().Format(time.RFC3339), id)
	return err
}
