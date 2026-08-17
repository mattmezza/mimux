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
	Active         bool
	AutoDisabledAt time.Time // zero = never auto-disabled
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

const webhookEndpointCols = `id, url, secret, events, active, auto_disabled_at, created_at`

func scanWebhookEndpoint(sc interface{ Scan(...any) error }) (WebhookEndpoint, error) {
	var e WebhookEndpoint
	var created string
	var disabled sql.NullString
	if err := sc.Scan(&e.ID, &e.URL, &e.Secret, &e.Events, &e.Active, &disabled, &created); err != nil {
		return e, err
	}
	e.CreatedAt, _ = time.Parse(time.RFC3339, created)
	e.AutoDisabledAt = parseNullTime(disabled)
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
// auto-disabled state). The secret is never rotated in place: to change it,
// delete the endpoint and create a new one, which is also what forces the new
// secret to be shown.
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
	CreatedAt      time.Time
	DeliveredAt    time.Time // zero until a 2xx
}

const webhookDeliveryCols = `id, endpoint_id, event_type, delivery_id, payload, status, attempts,
	next_attempt_at, last_status_code, last_error, created_at, delivered_at`

func scanWebhookDelivery(sc interface{ Scan(...any) error }) (WebhookDelivery, error) {
	var d WebhookDelivery
	var next, created string
	var delivered sql.NullString
	if err := sc.Scan(&d.ID, &d.EndpointID, &d.EventType, &d.DeliveryID, &d.Payload, &d.Status,
		&d.Attempts, &next, &d.LastStatusCode, &d.LastError, &created, &delivered); err != nil {
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
		last_status_code = ?, last_error = ?, delivered_at = ? WHERE id = ?`,
		d.Status, d.Attempts, d.NextAttemptAt.UTC().Format(time.RFC3339),
		d.LastStatusCode, d.LastError, nullTime(d.DeliveredAt), d.ID)
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
