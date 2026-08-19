//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/ext"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// The delivery engine. Endpoints are configured on the AGPL side (Settings →
// API, and the JSON API in api_webhooks.go); this file is what actually posts.
// The free build has the table and the UI and nothing that drains it, which is
// what the Settings copy says.

// webhookTick is how often due deliveries are drained. A fresh delivery is
// drained immediately when it is queued, so this interval only governs retries
// and anything left over from a previous run — which is why 5s (the outbox
// scheduler's number) is plenty.
//
// A var, not a const: the tests drive the engine by hand and must not race with
// the ambient one that routes() starts.
var webhookTick = 5 * time.Second

// webhookTimeout bounds one POST, end to end.
const webhookTimeout = 10 * time.Second

// webhookBatch caps how many deliveries one drain sends, so a backlog can't
// hold the loop for minutes.
const webhookBatch = 20

// webhookMaxRedirects is how far a redirect chain is followed. Two hops covers
// http→https and a path move; anything longer is a misconfigured receiver.
const webhookMaxRedirects = 2

// webhookResponseKeep is how much of a receiver's reply is stored for the
// deliveries screen. An error message fits; a stack trace or an HTML error page
// does not, and neither is worth keeping a hundred copies of.
const webhookResponseKeep = 2 << 10

// retryLadder is the delay before attempt N+1, indexed by the number of
// attempts already made: 0s, 1m, 5m, 30m, 2h, 8h, 14h. Seven attempts spread
// over ~24.5h, front-loaded because most failures are a receiver restarting and
// most of the rest are a receiver that is down for the day.
//
// When the ladder runs out the delivery is `dead` AND its endpoint is
// auto-disabled: a receiver that ignored us for a day is not coming back on its
// own, and the alternative — keep queueing into a black hole — buries the next
// real event in a hundred dead rows.
var retryLadder = []time.Duration{
	0, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 8 * time.Hour, 14 * time.Hour,
}

// listenBuffer is how far behind a live listener may fall before its stream
// starts dropping. A local receiver is fast and the tee is non-blocking by
// necessity (see tee), so this is the whole safety margin: a burst that outruns
// 64 is a receiver that has stopped reading.
const listenBuffer = 64

type webhooks struct {
	store   *store.Store
	mail    *mail.Manager
	cfg     *config.Config
	licence *licenceGate
	client  *http.Client
	tick    time.Duration
	ladder  []time.Duration

	mu        sync.Mutex
	listeners map[*listener]struct{}
}

// listener is one live stream — `mimux mail webhooks listen`, over
// GET /api/v1/webhooks/listen. It is deliberately nothing like an endpoint: no
// row, no retry, no subscription list to keep in sync. It sees every event the
// engine fires, because the point of it is to be zero-config.
type listener struct {
	ch      chan liveEvent
	events  map[string]bool // empty: everything
	dropped atomic.Int64
}

// liveEvent is one line of the stream. Payload is the exact bytes a receiver
// would be POSTed, so whatever forwards them signs the same string production
// signs.
type liveEvent struct {
	Type    string          `json:"type"` // event | ping | error
	Event   string          `json:"event,omitempty"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Dropped int64           `json:"dropped,omitempty"`
	Message string          `json:"message,omitempty"`
}

// newWebhooks takes the licence gate rather than reaching for one: without a
// valid licence this engine stops posting (see drain), and a required argument
// is the only way to make that impossible to forget at a call site.
func newWebhooks(deps ext.Deps, licence *licenceGate) *webhooks {
	return &webhooks{
		store:   deps.Store,
		mail:    deps.Mail,
		cfg:     deps.Cfg,
		licence: licence,
		client: &http.Client{
			Timeout: webhookTimeout,
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) > webhookMaxRedirects {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
		tick:      webhookTick,
		ladder:    retryLadder,
		listeners: map[*listener]struct{}{},
	}
}

// addListener registers a live stream and returns it with the func that
// unregisters it. events empty means everything.
func (e *webhooks) addListener(events []string) (*listener, func()) {
	l := &listener{ch: make(chan liveEvent, listenBuffer), events: map[string]bool{}}
	for _, ev := range events {
		if ev = strings.TrimSpace(ev); ev != "" {
			l.events[ev] = true
		}
	}
	e.mu.Lock()
	e.listeners[l] = struct{}{}
	e.mu.Unlock()
	return l, func() {
		e.mu.Lock()
		delete(e.listeners, l)
		e.mu.Unlock()
	}
}

// tee hands one event to every live listener, rendering the envelope only when
// somebody is actually listening.
//
// The send is non-blocking on purpose. This runs on the goroutine that reads
// the mail hub, and a blocked read there backs up through the hub's mutex into
// the sync engine itself — one `curl | head` would stall mail for the whole
// process. So a listener that has stopped reading loses events and is told how
// many, which is the honest version of what a local debugging stream is.
func (e *webhooks) tee(event string, data any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.listeners) == 0 {
		return
	}
	id, body, err := envelope(event, data)
	if err != nil {
		return // already logged by the caller's own envelope call, or about to be
	}
	for l := range e.listeners {
		if len(l.events) > 0 && !l.events[event] {
			continue
		}
		ev := liveEvent{Type: "event", Event: event, ID: id, Payload: json.RawMessage(body), Dropped: l.dropped.Load()}
		select {
		case l.ch <- ev:
		default:
			l.dropped.Add(1)
		}
	}
}

// run is the engine, in two goroutines: this one reads the hub and turns events
// into delivery rows (DB only, always fast), and drainLoop does the HTTP. They
// were one loop, which meant a drain — up to webhookBatch POSTs of up to
// webhookTimeout each — stopped the hub channel being read, and the hub drops
// events it cannot hand over. Nine new messages during one slow drain silently
// became nine webhooks nobody ever sent, with no replay to recover them. The
// subscription is durable on top of that: a burst that outruns even this reader
// makes the broadcaster wait rather than silently drop.
//
// Started from routes() and living as long as the process — ext.Extension has
// no shutdown hook and there is nothing to shut down: every piece of state is in
// SQLite, so a restart resumes mid-ladder.
func (e *webhooks) run(ctx context.Context) {
	events, unsubscribe := e.mail.SubscribeDurable()
	defer unsubscribe()
	// Capacity 1, dropped when full: the nudge says "there is something due",
	// not "there are N due", and drain reads the queue itself.
	nudge := make(chan struct{}, 1)
	go e.drainLoop(ctx, nudge)
	// Per-account "state + message", so a sync.error fires on the edge into
	// error rather than on every sync-status broadcast while it stays broken.
	seen := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			if e.translate(ev, seen) {
				select {
				case nudge <- struct{}{}: // the first attempt of the ladder is "now"
				default:
				}
			}
		}
	}
}

// drainLoop owns every outbound request: on a nudge from run, and on the ticker
// for retries and whatever the last run left mid-ladder.
func (e *webhooks) drainLoop(ctx context.Context, nudge <-chan struct{}) {
	t := time.NewTicker(e.tick)
	defer t.Stop()
	e.drain(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-nudge:
			e.drain(ctx)
		case <-t.C:
			e.drain(ctx)
		}
	}
}

// translate maps one hub event to zero or more webhook events, and reports
// whether anything was queued.
//
// message-new is the hub's "this message just arrived, here is its id" event;
// which webhook event it becomes is decided here, by the folder it landed in,
// so that policy stays out of the AGPL sync loop. message-updated is the same
// idea for a change to a message mimux already had — "<id> <change> <origin>" —
// and the folder is re-read, so a "moved" carries the destination. sync.error is
// derived from the sync-status event plus Manager.Status(), which already
// carries the error text the status bar shows — no new hub event needed for it.
//
// message-deleted is the exception to "the hub carries an id": the row is
// already gone when this runs, so the sync loop serialises the payload into the
// event and this only has to decide who gets it.
func (e *webhooks) translate(ev mail.Event, seen map[string]string) bool {
	switch ev.Type {
	case "message-new":
		id, err := strconv.ParseInt(ev.Data, 10, 64)
		if err != nil {
			return false
		}
		msg, err := e.store.MessageByID(id)
		if err != nil || msg == nil {
			return false
		}
		f, err := e.store.FolderByID(msg.FolderID)
		if err != nil || f == nil {
			return false
		}
		switch f.SpecialUse {
		case "inbox":
			return e.fire("message.received", receivedData(*msg, f))
		case "sent":
			return e.fire("message.sent", sentData(*msg))
		}
	case "message-updated":
		parts := strings.Fields(ev.Data)
		if len(parts) != 3 {
			return false
		}
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return false
		}
		// Gone between the change and here — a filter rule's move deletes the
		// local row, and the next sync can expunge one. Nothing to describe, so
		// nothing fires; same as an unknown id above.
		msg, err := e.store.MessageByID(id)
		if err != nil || msg == nil {
			return false
		}
		f, err := e.store.FolderByID(msg.FolderID)
		if err != nil || f == nil {
			return false
		}
		data := receivedData(*msg, f)
		data["change"] = parts[1]
		data["origin"] = parts[2]
		return e.fire("message.updated", data)
	case "message-deleted":
		// The payload rides in the event: the row is gone from the store by the
		// time this runs, so there is nothing left here to read it off.
		var data map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &data); err != nil {
			return false
		}
		return e.fire("message.deleted", data)
	case "sync-status":
		return e.syncErrors(e.mail.Status(), seen)
	}
	return false
}

// syncErrors fires sync.error for each account that has just gone into (or
// changed its reason for being in) the error state. Takes the statuses rather
// than reading them, so the edge detection can be tested without a live worker.
func (e *webhooks) syncErrors(statuses []mail.AccountStatus, seen map[string]string) bool {
	queued := false
	for _, st := range statuses {
		key := st.State + "\x00" + st.Message
		if seen[st.Account] == key {
			continue
		}
		seen[st.Account] = key
		if st.State != "error" {
			continue
		}
		if e.fire("sync.error", map[string]any{"account": st.Account, "error": st.Message}) {
			queued = true
		}
	}
	return queued
}

// receivedData is the message.received payload: a summary. Never a body —
// bodies are large, and a webhook is the one place mail leaves this machine
// without the user watching.
func receivedData(m store.Message, f *store.Folder) map[string]any {
	return map[string]any{
		"id":         m.ID,
		"account":    m.Account,
		"folder":     f.Name,
		"folder_id":  f.ID,
		"from":       addrJSON{Name: m.FromName, Address: m.FromAddress},
		"subject":    m.Subject,
		"date":       m.Date.UTC().Format(time.RFC3339),
		"snippet":    m.Snippet,
		"message_id": m.MessageID,
	}
}

// sentData is the message.sent payload, read off the copy that lands in the
// Sent folder.
func sentData(m store.Message) map[string]any {
	return map[string]any{
		"id":         m.ID,
		"account":    m.Account,
		"to":         mail.SplitAddrList(m.ToAddresses),
		"subject":    m.Subject,
		"date":       m.Date.UTC().Format(time.RFC3339),
		"message_id": m.MessageID,
	}
}

// fire queues one event for every active endpoint subscribed to it, and hands
// it to every live listener regardless of what any endpoint is subscribed to.
func (e *webhooks) fire(event string, data any) bool {
	e.tee(event, data)
	eps, err := e.store.ListWebhookEndpoints()
	if err != nil {
		slog.Error("webhooks: list endpoints", "err", err)
		return false
	}
	queued := false
	for i := range eps {
		if !eps[i].Active || !eps[i].Wants(event) {
			continue
		}
		if e.queue(&eps[i], event, data) != nil {
			queued = true
		}
	}
	return queued
}

// queue writes one delivery row and returns it, or nil if it could not be
// written. The body is rendered once and stored verbatim: every attempt signs
// and sends exactly these bytes, so a receiver can verify a retry the same way
// it verified the first try.
func (e *webhooks) queue(ep *store.WebhookEndpoint, event string, data any) *store.WebhookDelivery {
	id, body, err := envelope(event, data)
	if err != nil {
		slog.Error("webhooks: encode payload", "event", event, "err", err) // never the payload
		return nil
	}
	d := &store.WebhookDelivery{
		EndpointID: ep.ID, EventType: event, DeliveryID: id, Payload: body,
	}
	if err := e.store.EnqueueWebhookDelivery(d); err != nil {
		slog.Error("webhooks: enqueue", "endpoint", ep.ID, "event", event, "err", err)
		return nil
	}
	return d
}

// envelope renders one event as the bytes a receiver gets: a fresh delivery id
// and the JSON body that gets signed. Shared by queue, which stores it against
// a delivery row, and tee, which streams it to a live listener — so a local
// listener sees byte-for-byte what production posts.
func envelope(event string, data any) (id, body string, err error) {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	id = hex.EncodeToString(buf)
	b, err := json.Marshal(map[string]any{
		"id":         id,
		"event":      event,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"data":       data,
	})
	return id, string(b), err
}

// drain sends every delivery whose next attempt has come.
//
// An unlicensed install drains nothing. The lapse acts exactly like the pause
// switch an endpoint already has, but global: events keep translating into
// delivery rows, the rows stay pending, and the first drain after a valid key
// lands sends them. Checked here and not in translate/queue — the alternative,
// dropping events on the floor, loses what no receiver can ask for again — and
// deliberately not counted as a failed attempt, because a lapsed licence must
// not walk the retry ladder and auto-disable every endpoint the operator owns.
func (e *webhooks) drain(ctx context.Context) {
	if s := e.licence.current(time.Now()); !s.Allowed {
		return
	}
	due, err := e.store.DueWebhookDeliveries(time.Now(), webhookBatch)
	if err != nil {
		slog.Error("webhooks: due", "err", err)
		return
	}
	for i := range due {
		d := &due[i]
		ep, err := e.store.WebhookEndpointByID(d.EndpointID)
		if err != nil || ep == nil {
			continue
		}
		// A paused or auto-disabled endpoint keeps its queue: the rows stay
		// pending and go out when it is switched back on.
		if !ep.Active {
			continue
		}
		e.attempt(ctx, ep, d)
	}
}

// attempt makes one delivery attempt and records the outcome.
//
// Retry policy: 2xx is done. 410 Gone is the one refusal that means "stop" —
// it is the standard "this subscription is over" answer, and it is what the
// push-notification path already honours. Everything else — timeouts, 5xx,
// 429, and the other 4xx, because a misconfigured receiver is usually
// misconfigured temporarily — walks the ladder and then dies.
func (e *webhooks) attempt(ctx context.Context, ep *store.WebhookEndpoint, d *store.WebhookDelivery) {
	started := time.Now()
	code, body, err := e.post(ctx, ep, d)
	d.Attempts++
	d.LastStatusCode = code
	d.DurationMS = int(time.Since(started).Milliseconds())
	d.ResponseBody = body
	d.LastError = ""
	if err != nil {
		d.LastError = truncate(err.Error(), 300)
	}
	exhausted := false
	switch {
	case code >= 200 && code < 300:
		d.Status, d.DeliveredAt = store.WebhookOK, time.Now().UTC()
	case code == http.StatusGone:
		d.Status = store.WebhookDead
		d.LastError = "endpoint replied 410 Gone"
	case d.Attempts >= len(e.ladder):
		d.Status, exhausted = store.WebhookDead, true
	default:
		d.Status = store.WebhookFailed
		d.NextAttemptAt = time.Now().Add(e.ladder[d.Attempts])
	}
	if err := e.store.SaveWebhookDelivery(d); err != nil {
		slog.Error("webhooks: save delivery", "endpoint", ep.ID, "err", err)
	}
	if exhausted {
		if err := e.store.AutoDisableWebhookEndpoint(ep.ID, time.Now()); err != nil {
			slog.Error("webhooks: auto-disable", "endpoint", ep.ID, "err", err)
		} else {
			slog.Warn("webhooks: endpoint auto-disabled after a delivery exhausted every retry",
				"endpoint", ep.ID, "event", d.EventType)
		}
	}
	// Logged: which endpoint, which event, how it went. Never the payload, the
	// secret or the signature.
	if d.Status != store.WebhookOK {
		slog.Warn("webhooks: delivery failed", "endpoint", ep.ID, "event", d.EventType,
			"status", d.Status, "code", code, "attempts", d.Attempts)
	}
	// A delivery that is never coming back is worth an email; a receiver that is
	// down all day is not worth forty of them. See notifyFailure.
	if d.Status == store.WebhookDead {
		e.notifyFailure(ctx, ep, d)
	}
}

// post sends one attempt and returns the response status (0 when the request
// never got an answer) and the first bytes of what came back.
func (e *webhooks) post(ctx context.Context, ep *store.WebhookEndpoint, d *store.WebhookDelivery) (int, string, error) {
	// The store refuses anything else on the way in; this is the second lock on
	// the same door, because this is the line that actually dials out.
	if !strings.HasPrefix(ep.URL, "http://") && !strings.HasPrefix(ep.URL, "https://") {
		return 0, "", errors.New("refusing to POST to a non-http(s) URL")
	}
	ctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, strings.NewReader(d.Payload))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mimux-webhooks/1")
	req.Header.Set("X-Mimux-Event", d.EventType)
	req.Header.Set("X-Mimux-Delivery-Id", d.DeliveryID)
	req.Header.Set("X-Mimux-Signature", signature(ep.Secret, time.Now().Unix(), d.Payload))
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	// Read a little of the body: enough for the connection to be reused, and
	// enough for "HTTP 400" to become "HTTP 400: unknown event type" on the
	// deliveries screen. The rest is drained and dropped.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, webhookResponseKeep))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode, strings.ToValidUTF8(string(body), ""), nil
}

// notifyFailure emails the user that an endpoint is dropping deliveries — at
// most once a day per endpoint, counted from the last one actually sent.
//
// The stamp is written before the send, not after: a mail server that hangs for
// the whole timeout must not become a second email on the next dead delivery.
// Sent from (and to) the first configured account, which is the mailbox this
// person is reading in mimux; an install with no account yet gets no mail and
// no error, because there is nothing to send it with.
func (e *webhooks) notifyFailure(ctx context.Context, ep *store.WebhookEndpoint, d *store.WebhookDelivery) {
	now := time.Now()
	if !ep.FailureEmailDue(now) {
		return
	}
	accts := e.cfg.Accounts
	if len(accts) == 0 {
		return
	}
	to := accts[0].Email
	if to == "" {
		return
	}
	if err := e.store.MarkWebhookFailureEmail(ep.ID, now); err != nil {
		slog.Warn("webhooks: stamping failure email", "endpoint", ep.ID, "err", err)
		return // no stamp, no send: an unstamped send is an unbounded send
	}
	go func() {
		// Markdown, so the message carries the plain text as written and the
		// HTML part the house renderer builds from it — the same pair every
		// other mimux message ships.
		_, err := e.mail.Send(context.WithoutCancel(ctx), accts[0].Name, mail.ComposeInput{
			To:      []string{to},
			Subject: "mimux: a webhook is failing",
			Mode:    "markdown",
			Body:    failureMailBody(ep, d, e.cfg.Server.BaseURL),
		})
		if err != nil {
			slog.Warn("webhooks: failure email", "endpoint", ep.ID, "err", err)
		}
	}()
}

// failureMailBody is the failure notice. It says which endpoint, what happened
// and where to look — never the payload, which is mail metadata that has no
// business being re-sent through a second channel.
func failureMailBody(ep *store.WebhookEndpoint, d *store.WebhookDelivery, baseURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A webhook delivery to **%s** gave up after %d attempts.\n\n",
		ep.URL, d.Attempts)
	fmt.Fprintf(&b, "- Event: `%s`\n", d.EventType)
	fmt.Fprintf(&b, "- Delivery: `%s`\n", d.DeliveryID)
	if d.LastStatusCode != 0 {
		fmt.Fprintf(&b, "- Last response: HTTP %d\n", d.LastStatusCode)
	}
	if d.LastError != "" {
		fmt.Fprintf(&b, "- Last error: %s\n", d.LastError)
	}
	b.WriteString("\n")
	if !ep.Active {
		b.WriteString("The endpoint has been switched off, so nothing more is queued for it. " +
			"Fix the receiver, then press Resume in Settings.\n\n")
	}
	if baseURL != "" {
		fmt.Fprintf(&b, "The full log, with the request body and the response: %s/settings/webhooks/%d/deliveries\n\n",
			strings.TrimRight(baseURL, "/"), ep.ID)
	}
	b.WriteString("You will not get another message about this endpoint for 24 hours, " +
		"however many deliveries fail in the meantime.\n")
	return b.String()
}

// signature builds the X-Mimux-Signature header: `t=<unix>,v1=<hex>` where the
// hex is HMAC-SHA256(secret, "<t>.<body>"). The timestamp is inside the signed
// string, so a receiver that rejects an old t cannot be fooled by re-stamping a
// captured delivery.
func signature(secret string, t int64, body string) string {
	ts := strconv.FormatInt(t, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + body))
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// truncate caps a stored error string, cutting on runes so a multi-byte
// character can't be sliced in half on its way into the log.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
