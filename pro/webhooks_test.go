//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/ext"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// TestMain slows the ambient engine that routes() starts down to a tick nothing
// in a test run will reach: the tests here drive their own engine explicitly,
// and a background ticker firing against a closed test store proves nothing.
func TestMain(m *testing.M) {
	webhookTick = time.Hour
	os.Exit(m.Run())
}

// testEngine is an engine over a fresh store, with a ladder short enough to
// walk in a test.
func testEngine(t *testing.T, ladder ...time.Duration) (*webhooks, *store.Store, *mail.Manager) {
	t.Helper()
	st := openStore(t)
	cfg := &config.Config{}
	m := mail.NewManager(cfg, st)
	deps := ext.Deps{Mail: m, Store: st, Cfg: cfg}
	e := newWebhooks(deps, newLicenceGate(deps))
	e.tick = 10 * time.Millisecond
	if len(ladder) > 0 {
		e.ladder = ladder
	}
	return e, st, m
}

func seedEndpoint(t *testing.T, st *store.Store, url, events string) *store.WebhookEndpoint {
	t.Helper()
	ep := &store.WebhookEndpoint{URL: url, Secret: "topsecret", Events: events, Active: true}
	if err := st.CreateWebhookEndpoint(ep); err != nil {
		t.Fatal(err)
	}
	return ep
}

// recorder is a webhook receiver that records what it was sent and answers with
// a scripted status code.
type recorder struct {
	mu     sync.Mutex
	bodies []string
	heads  []http.Header
	code   int
	got    chan struct{}
}

func newRecorder(code int) *recorder { return &recorder{code: code, got: make(chan struct{}, 16)} }

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.bodies = append(rc.bodies, string(body))
	rc.heads = append(rc.heads, r.Header.Clone())
	code := rc.code
	rc.mu.Unlock()
	w.WriteHeader(code)
	select {
	case rc.got <- struct{}{}:
	default:
	}
}

func (rc *recorder) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.bodies)
}

func (rc *recorder) last() (string, http.Header) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.bodies) == 0 {
		return "", nil
	}
	return rc.bodies[len(rc.bodies)-1], rc.heads[len(rc.heads)-1]
}

// TestWebhookSignatureVector pins the wire format against a value computed
// outside this codebase: HMAC-SHA256("topsecret", "1700000000." + body). A
// receiver written from the docs must keep verifying after any refactor here.
func TestWebhookSignatureVector(t *testing.T) {
	const want = "t=1700000000,v1=79883357e4c4c4abee43cf4b32367d67a1344520479e3e8c85e98406a6d6a2a5"
	if got := signature("topsecret", 1700000000, `{"hello":"world"}`); got != want {
		t.Errorf("signature =\n%s\nwant\n%s", got, want)
	}
	// A different secret, timestamp or body must all change it.
	if signature("other", 1700000000, `{"hello":"world"}`) == want ||
		signature("topsecret", 1700000001, `{"hello":"world"}`) == want ||
		signature("topsecret", 1700000000, `{"hello":"there"}`) == want {
		t.Error("signature is insensitive to one of its inputs")
	}
}

// verify is the check a receiver performs, written the way the docs describe it.
func verify(t *testing.T, secret, header, body string) {
	t.Helper()
	var ts, v1 string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			v1 = v
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(sec, 0)) > 5*time.Minute {
		t.Fatalf("timestamp %q is missing or stale", ts)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + body))
	if !hmac.Equal([]byte(v1), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		t.Fatalf("signature %q does not verify over the body", v1)
	}
}

func TestWebhookDeliverySucceeds(t *testing.T) {
	rc := newRecorder(http.StatusNoContent)
	srv := httptest.NewServer(rc)
	defer srv.Close()

	e, st, _ := testEngine(t)
	ep := seedEndpoint(t, st, srv.URL, "message.received")
	if !e.fire("message.received", map[string]any{"subject": "hi"}) {
		t.Fatal("fire queued nothing")
	}
	e.drain(context.Background())

	if rc.count() != 1 {
		t.Fatalf("receiver got %d deliveries, want 1", rc.count())
	}
	body, head := rc.last()
	verify(t, ep.Secret, head.Get("X-Mimux-Signature"), body)
	if head.Get("X-Mimux-Event") != "message.received" || head.Get("X-Mimux-Delivery-Id") == "" {
		t.Errorf("headers = %v", head)
	}
	var got struct {
		ID, Event, CreatedAt string
		Data                 map[string]any
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not the documented envelope: %v\n%s", err, body)
	}
	if got.Event != "message.received" || got.ID != head.Get("X-Mimux-Delivery-Id") || got.Data["subject"] != "hi" {
		t.Errorf("body = %s", body)
	}

	log, _ := st.ListWebhookDeliveries(ep.ID, 10)
	if len(log) != 1 || log[0].Status != store.WebhookOK || log[0].Attempts != 1 || log[0].DeliveredAt.IsZero() {
		t.Fatalf("delivery not marked ok: %+v", log)
	}
}

// TestWebhookLicenceLapseHoldsTheQueue: a lapsed licence is a global pause. The
// event still becomes a delivery row, nothing goes out, no attempt is spent
// (so the ladder cannot auto-disable an endpoint over an unpaid invoice), and
// the first drain after a valid key lands sends the backlog.
func TestWebhookLicenceLapseHoldsTheQueue(t *testing.T) {
	rc := newRecorder(http.StatusOK)
	srv := httptest.NewServer(rc)
	defer srv.Close()

	e, st, _ := testEngine(t)
	ep := seedEndpoint(t, st, srv.URL, "message.received")
	// Expired, and well past the grace period.
	e.cfg.LicenceKey = annualKey(t, time.Now().AddDate(0, 0, -(graceDays + 30)))
	e.licence.forceRecheck()

	if !e.fire("message.received", map[string]any{"subject": "hi"}) {
		t.Fatal("an unlicensed engine stopped queueing events")
	}
	e.drain(context.Background())
	if rc.count() != 0 {
		t.Fatalf("an expired licence delivered %d webhooks", rc.count())
	}
	log, _ := st.ListWebhookDeliveries(ep.ID, 10)
	if len(log) != 1 || log[0].Status != store.WebhookPending || log[0].Attempts != 0 {
		t.Fatalf("the held delivery is not untouched: %+v", log)
	}
	if back, _ := st.WebhookEndpointByID(ep.ID); back == nil || !back.Active {
		t.Fatal("a licence lapse disabled the endpoint")
	}

	e.cfg.LicenceKey = annualKey(t, time.Now().AddDate(0, 6, 0))
	e.licence.forceRecheck()
	e.drain(context.Background())
	if rc.count() != 1 {
		t.Fatalf("a renewed licence drained %d webhooks, want 1", rc.count())
	}
	log, _ = st.ListWebhookDeliveries(ep.ID, 10)
	if len(log) != 1 || log[0].Status != store.WebhookOK {
		t.Fatalf("the held delivery did not go out: %+v", log)
	}
}

// TestWebhookRetryLadderAndAutoDisable: a receiver that never recovers gets one
// attempt per rung, then the delivery dies and the endpoint is switched off so
// the next event isn't queued into a black hole.
func TestWebhookRetryLadderAndAutoDisable(t *testing.T) {
	rc := newRecorder(http.StatusInternalServerError)
	srv := httptest.NewServer(rc)
	defer srv.Close()

	e, st, _ := testEngine(t, 0, 0, 0) // three rungs, no waiting
	ep := seedEndpoint(t, st, srv.URL, "ping")
	e.queue(ep, "ping", map[string]any{})

	for range 5 { // more drains than rungs: the extra ones must be no-ops
		e.drain(context.Background())
	}
	if rc.count() != 3 {
		t.Fatalf("receiver got %d attempts, want one per rung (3)", rc.count())
	}
	log, _ := st.ListWebhookDeliveries(ep.ID, 10)
	if len(log) != 1 || log[0].Status != store.WebhookDead || log[0].Attempts != 3 {
		t.Fatalf("delivery did not die at the end of the ladder: %+v", log)
	}
	if log[0].LastStatusCode != 500 {
		t.Errorf("last status code = %d", log[0].LastStatusCode)
	}
	got, _ := st.WebhookEndpointByID(ep.ID)
	if got.Active || !got.AutoDisabled() {
		t.Fatalf("endpoint was not auto-disabled: %+v", got)
	}
	// And a disabled endpoint stops receiving events entirely.
	if e.fire("ping", map[string]any{}) {
		t.Error("an auto-disabled endpoint was still queued for")
	}
}

// TestWebhook410IsFinal: 410 Gone means the subscription is over — one attempt,
// no ladder, and the endpoint is left alone (the receiver chose this).
func TestWebhook410IsFinal(t *testing.T) {
	rc := newRecorder(http.StatusGone)
	srv := httptest.NewServer(rc)
	defer srv.Close()

	e, st, _ := testEngine(t, 0, 0, 0)
	ep := seedEndpoint(t, st, srv.URL, "ping")
	e.queue(ep, "ping", map[string]any{})
	for range 3 {
		e.drain(context.Background())
	}
	if rc.count() != 1 {
		t.Fatalf("receiver got %d attempts after 410, want 1", rc.count())
	}
	log, _ := st.ListWebhookDeliveries(ep.ID, 10)
	if log[0].Status != store.WebhookDead || log[0].LastStatusCode != http.StatusGone {
		t.Fatalf("410 did not kill the delivery immediately: %+v", log[0])
	}
	if got, _ := st.WebhookEndpointByID(ep.ID); !got.Active {
		t.Error("410 on one delivery disabled the whole endpoint")
	}
}

// TestWebhookRetriesThenSucceeds: a receiver that comes back is delivered to,
// with the same delivery id (at-least-once, deduplicated by the receiver).
func TestWebhookRetriesThenSucceeds(t *testing.T) {
	rc := newRecorder(http.StatusBadGateway)
	srv := httptest.NewServer(rc)
	defer srv.Close()

	e, st, _ := testEngine(t, 0, 0, 0)
	ep := seedEndpoint(t, st, srv.URL, "ping")
	e.queue(ep, "ping", map[string]any{})
	e.drain(context.Background())
	rc.mu.Lock()
	rc.code = http.StatusOK
	rc.mu.Unlock()
	e.drain(context.Background())

	log, _ := st.ListWebhookDeliveries(ep.ID, 10)
	if log[0].Status != store.WebhookOK || log[0].Attempts != 2 {
		t.Fatalf("recovery not recorded: %+v", log[0])
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.heads[0].Get("X-Mimux-Delivery-Id") != rc.heads[1].Get("X-Mimux-Delivery-Id") {
		t.Error("the retry used a different delivery id, so a receiver cannot deduplicate")
	}
	if rc.bodies[0] != rc.bodies[1] {
		t.Error("the retry sent a different body")
	}
}

// TestWebhookTranslatesHubEvents is the hub-event → delivery-row mapping: which
// folder the new message landed in decides the event, and nothing else fires.
func TestWebhookTranslatesHubEvents(t *testing.T) {
	e, st, _ := testEngine(t)
	ep := seedEndpoint(t, st, "https://example.test/hook", "message.received message.sent")
	inbox := seedFolder(t, st, "a1", "INBOX", "inbox")
	sent := seedFolder(t, st, "a1", "Sent", "sent")
	archive := seedFolder(t, st, "a1", "Archive", "archive")

	got := seedMsg(t, st, store.Message{Account: "a1", FolderID: inbox, UID: 1, Subject: "Hello",
		FromName: "Ada", FromAddress: "ada@example.test", Snippet: "the snippet", MessageID: "<1@x>"})
	out := seedMsg(t, st, store.Message{Account: "a1", FolderID: sent, UID: 2, Subject: "Re: Hello",
		ToAddresses: "ada@example.test, bob@example.test", MessageID: "<2@x>"})
	filed := seedMsg(t, st, store.Message{Account: "a1", FolderID: archive, UID: 3, Subject: "Old"})

	seen := map[string]string{}
	for _, id := range []int64{got, out, filed} {
		e.translate(mail.Event{Type: "message-new", Data: strconv.FormatInt(id, 10)}, seen)
	}
	// Junk ids and unrelated event types must not queue anything either.
	e.translate(mail.Event{Type: "message-new", Data: "not-a-number"}, seen)
	e.translate(mail.Event{Type: "message-new", Data: "999999"}, seen)
	e.translate(mail.Event{Type: "toast", Data: "hi"}, seen)

	log, _ := st.ListWebhookDeliveries(ep.ID, 10)
	if len(log) != 2 {
		t.Fatalf("queued %d deliveries, want 2 (inbox + sent, not the archived one)", len(log))
	}
	// Newest first: the Sent copy, then the received one.
	if log[0].EventType != "message.sent" || log[1].EventType != "message.received" {
		t.Fatalf("events = %q, %q", log[0].EventType, log[1].EventType)
	}
	var received struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(log[1].Payload), &received); err != nil {
		t.Fatal(err)
	}
	d := received.Data
	if d["subject"] != "Hello" || d["folder"] != "INBOX" || d["snippet"] != "the snippet" {
		t.Errorf("message.received payload = %v", d)
	}
	if from, _ := d["from"].(map[string]any); from["address"] != "ada@example.test" {
		t.Errorf("from = %v", d["from"])
	}
	// Summaries only: no body field, whatever the message holds.
	if _, ok := d["body"]; ok {
		t.Error("message.received payload carries a body")
	}
	var sentPayload struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(log[0].Payload), &sentPayload); err != nil {
		t.Fatal(err)
	}
	if to, _ := sentPayload.Data["to"].([]any); len(to) != 2 || to[0] != "ada@example.test" {
		t.Errorf("message.sent payload to = %v", sentPayload.Data["to"])
	}

	// An endpoint that didn't subscribe hears nothing.
	quiet := seedEndpoint(t, st, "https://quiet.test/hook", "sync.error")
	e.translate(mail.Event{Type: "message-new", Data: strconv.FormatInt(got, 10)}, seen)
	if log, _ := st.ListWebhookDeliveries(quiet.ID, 10); len(log) != 0 {
		t.Errorf("unsubscribed endpoint got %d deliveries", len(log))
	}
}

// TestWebhookTranslatesMessageUpdated: the hub's "<id> <change> <origin>"
// becomes a message.updated carrying the message as it is now, for subscribers
// only.
func TestWebhookTranslatesMessageUpdated(t *testing.T) {
	e, st, _ := testEngine(t)
	ep := seedEndpoint(t, st, "https://example.test/hook", "message.updated")
	// Not the inbox: message.updated is about the change, not where it landed.
	archive := seedFolder(t, st, "a1", "Archive", "archive")
	quiet := seedEndpoint(t, st, "https://quiet.test/hook", "message.received message.sent")

	id := seedMsg(t, st, store.Message{Account: "a1", FolderID: archive, UID: 1, Subject: "Hello",
		FromName: "Ada", FromAddress: "ada@example.test", MessageID: "<1@x>"})

	seen := map[string]string{}
	e.translate(mail.Event{Type: "message-updated", Data: strconv.FormatInt(id, 10) + " moved mimux"}, seen)
	// Malformed data, and a message that is gone by the time we look, queue
	// nothing.
	e.translate(mail.Event{Type: "message-updated", Data: strconv.FormatInt(id, 10) + " moved"}, seen)
	e.translate(mail.Event{Type: "message-updated", Data: "not-a-number read mimux"}, seen)
	e.translate(mail.Event{Type: "message-updated", Data: "999999 moved external"}, seen)

	log, _ := st.ListWebhookDeliveries(ep.ID, 10)
	if len(log) != 1 {
		t.Fatalf("queued %d deliveries, want 1", len(log))
	}
	if log[0].EventType != "message.updated" {
		t.Fatalf("event = %q", log[0].EventType)
	}
	var p struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(log[0].Payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.Data["change"] != "moved" || p.Data["origin"] != "mimux" || p.Data["folder"] != "Archive" || p.Data["subject"] != "Hello" {
		t.Errorf("message.updated payload = %v", p.Data)
	}
	if _, ok := p.Data["body"]; ok {
		t.Error("message.updated payload carries a body")
	}
	if log, _ := st.ListWebhookDeliveries(quiet.ID, 10); len(log) != 0 {
		t.Errorf("unsubscribed endpoint got %d deliveries", len(log))
	}
}

// TestWebhookTranslatesMessageDeleted: the row is gone by the time this runs,
// so the payload rides in the event and only the subscribers get it.
func TestWebhookTranslatesMessageDeleted(t *testing.T) {
	e, st, _ := testEngine(t)
	ep := seedEndpoint(t, st, "https://example.test/hook", "message.deleted")
	quiet := seedEndpoint(t, st, "https://quiet.test/hook", "message.received message.updated")

	seen := map[string]string{}
	e.translate(mail.Event{Type: "message-deleted", Data: `{"id":7,"account":"a1","folder":"INBOX","subject":"gone"}`}, seen)
	e.translate(mail.Event{Type: "message-deleted", Data: "not json"}, seen)

	log, _ := st.ListWebhookDeliveries(ep.ID, 10)
	if len(log) != 1 {
		t.Fatalf("queued %d deliveries, want 1", len(log))
	}
	if log[0].EventType != "message.deleted" {
		t.Fatalf("event = %q", log[0].EventType)
	}
	var p struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(log[0].Payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.Data["subject"] != "gone" || p.Data["folder"] != "INBOX" {
		t.Errorf("message.deleted payload = %v", p.Data)
	}
	if log, _ := st.ListWebhookDeliveries(quiet.ID, 10); len(log) != 0 {
		t.Errorf("unsubscribed endpoint got %d deliveries", len(log))
	}
}

// TestWebhookSyncErrorEdges: an account that stays broken must not re-fire on
// every sync-status broadcast, but a new reason must.
func TestWebhookSyncErrorEdges(t *testing.T) {
	e, st, _ := testEngine(t)
	ep := seedEndpoint(t, st, "https://example.test/hook", "sync.error")
	seen := map[string]string{}

	ok := []mail.AccountStatus{{Account: "a1", State: "ok"}}
	broken := []mail.AccountStatus{{Account: "a1", State: "error", Message: "auth failed"}}
	otherBreak := []mail.AccountStatus{{Account: "a1", State: "error", Message: "connection refused"}}

	e.syncErrors(ok, seen)
	if log, _ := st.ListWebhookDeliveries(ep.ID, 10); len(log) != 0 {
		t.Fatalf("a healthy account fired sync.error: %+v", log)
	}
	e.syncErrors(broken, seen)
	e.syncErrors(broken, seen) // still broken, same reason: no second event
	e.syncErrors(otherBreak, seen)
	e.syncErrors(ok, seen)

	log, _ := st.ListWebhookDeliveries(ep.ID, 10)
	if len(log) != 2 {
		t.Fatalf("queued %d sync.error deliveries, want 2 (one per distinct failure)", len(log))
	}
	var p struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(log[1].Payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.Data["account"] != "a1" || p.Data["error"] != "auth failed" {
		t.Errorf("sync.error payload = %v", p.Data)
	}
}

// TestWebhookEngineRunsOffTheHub is the wiring proof: a broadcast on the mail
// manager's own hub reaches a real HTTP receiver, through run().
func TestWebhookEngineRunsOffTheHub(t *testing.T) {
	rc := newRecorder(http.StatusOK)
	srv := httptest.NewServer(rc)
	defer srv.Close()

	e, st, m := testEngine(t)
	seedEndpoint(t, st, srv.URL, "message.received")
	inbox := seedFolder(t, st, "a1", "INBOX", "inbox")
	id := seedMsg(t, st, store.Message{Account: "a1", FolderID: inbox, UID: 1, Subject: "Ping"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.run(ctx)
	// The engine has to be subscribed before the broadcast: the hub does not
	// replay. Broadcasting until it lands is the honest way to wait for that.
	deadline := time.After(2 * time.Second)
	for {
		m.Broadcast("message-new", strconv.FormatInt(id, 10))
		select {
		case <-rc.got:
			if _, head := rc.last(); head.Get("X-Mimux-Event") != "message.received" {
				t.Fatalf("delivered the wrong event: %v", head)
			}
			return
		case <-deadline:
			t.Fatal("a hub broadcast never reached the receiver")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestWebhookRefusesNonHTTPURL: the store blocks these on the way in, and the
// sender blocks them again on the way out.
func TestWebhookRefusesNonHTTPURL(t *testing.T) {
	e, _, _ := testEngine(t)
	ep := &store.WebhookEndpoint{ID: 1, URL: "file:///etc/passwd", Secret: "s"}
	d := &store.WebhookDelivery{EventType: "ping", DeliveryID: "x", Payload: "{}"}
	if code, _, err := e.post(context.Background(), ep, d); err == nil || code != 0 {
		t.Errorf("post to %q = %d, %v; want a refusal", ep.URL, code, err)
	}
}

// TestWebhookRecordsResponseAndTiming: the deliveries screen is only useful if
// the engine keeps what the receiver said and how long it took saying it.
func TestWebhookRecordsResponseAndTiming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("unknown event type"))
	}))
	defer srv.Close()

	e, st, _ := testEngine(t, time.Hour) // one rung: it dies on the first attempt
	ep := seedEndpoint(t, st, srv.URL, "message.received")
	e.fire("message.received", map[string]any{"subject": "hi"})
	e.drain(context.Background())

	log, _ := st.ListWebhookDeliveries(ep.ID, 10)
	if len(log) != 1 {
		t.Fatalf("log = %+v", log)
	}
	d := log[0]
	if d.LastStatusCode != http.StatusBadRequest || d.ResponseBody != "unknown event type" {
		t.Errorf("response not recorded: code %d, body %q", d.LastStatusCode, d.ResponseBody)
	}
	if d.DurationMS < 0 {
		t.Errorf("duration = %d ms", d.DurationMS)
	}
}

// TestWebhookFailureEmailIsDedupedPerDay: a receiver that is down all day
// produces one email, not one per dead delivery. The stamp is what enforces it,
// and it is written whether or not the SMTP send then works — a mail server
// that is also down must not turn into an unbounded retry of its own.
func TestWebhookFailureEmailIsDedupedPerDay(t *testing.T) {
	e, st, _ := testEngine(t)
	// One account, so notifyFailure has somewhere to send. The manager has no
	// live connection for it, so Send fails immediately and nothing dials out.
	e.cfg.Accounts = []config.Account{{Name: "acct", Email: "me@example.test"}}
	ep := seedEndpoint(t, st, "https://example.test/hook", "message.received")
	d := &store.WebhookDelivery{EndpointID: ep.ID, EventType: "message.received", DeliveryID: "x", Payload: "{}"}
	if err := st.EnqueueWebhookDelivery(d); err != nil {
		t.Fatal(err)
	}
	d.Status, d.Attempts = store.WebhookDead, 7

	e.notifyFailure(context.Background(), ep, d)
	first, _ := st.WebhookEndpointByID(ep.ID)
	if first.FailureEmailAt.IsZero() {
		t.Fatal("the first dead delivery did not stamp a failure email")
	}

	// A second dead delivery the same day: the stamp is what the engine reads,
	// so hand it the fresh endpoint rather than the stale struct.
	e.notifyFailure(context.Background(), first, d)
	second, _ := st.WebhookEndpointByID(ep.ID)
	if !second.FailureEmailAt.Equal(first.FailureEmailAt) {
		t.Errorf("a second failure the same day sent another email: %v then %v",
			first.FailureEmailAt, second.FailureEmailAt)
	}

	// A day later it is due again.
	if err := st.MarkWebhookFailureEmail(ep.ID, time.Now().Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	old, _ := st.WebhookEndpointByID(ep.ID)
	e.notifyFailure(context.Background(), old, d)
	again, _ := st.WebhookEndpointByID(ep.ID)
	if !again.FailureEmailAt.After(old.FailureEmailAt) {
		t.Errorf("a failure a day later did not send: %v", again.FailureEmailAt)
	}
}

// TestWebhookQueuesWhileDraining is the drop-window regression: a receiver that
// never answers holds a drain for its full timeout, and while it does, events
// arriving on the hub must still become delivery rows. When reading the hub and
// sending were the same loop this queued exactly one row and then went deaf
// until the receiver timed out.
func TestWebhookQueuesWhileDraining(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // hold the drain open for the whole test
		w.WriteHeader(http.StatusOK)
	}))
	// LIFO: cancel the engine, let the held requests go, then shut the server.
	defer srv.Close()
	defer close(release)

	e, st, m := testEngine(t)
	ep := seedEndpoint(t, st, srv.URL, "message.received")
	inbox := seedFolder(t, st, "a1", "INBOX", "inbox")
	const n = 30
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = seedMsg(t, st, store.Message{
			Account: "a1", FolderID: inbox, UID: uint32(i + 1), Subject: "Ping",
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.run(ctx)

	// One event at a time, waiting for its row: the hub buffer is small and does
	// not replay, so a burst would be testing the hub's drop policy instead of
	// this engine's. The first id is re-broadcast until it lands, which is how
	// the test waits for run() to have subscribed (and is why the log may hold a
	// few duplicates of it).
	deadline := time.Now().Add(10 * time.Second)
	wait := func(before int) {
		t.Helper()
		for countDeliveries(t, st, ep.ID) <= before {
			if time.Now().After(deadline) {
				t.Fatalf("only %d of %d events became delivery rows: the engine stopped reading the hub while draining",
					countDeliveries(t, st, ep.ID), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	for countDeliveries(t, st, ep.ID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a hub broadcast never became a delivery row")
		}
		m.Broadcast("message-new", strconv.FormatInt(ids[0], 10))
		time.Sleep(5 * time.Millisecond)
	}
	for _, id := range ids[1:] {
		before := countDeliveries(t, st, ep.ID)
		m.Broadcast("message-new", strconv.FormatInt(id, 10))
		wait(before)
	}

	// Every message is in the log, not just the right number of rows.
	log, err := st.ListWebhookDeliveries(ep.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, d := range log {
		var p struct {
			Data struct {
				ID int64 `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(d.Payload), &p); err != nil {
			t.Fatal(err)
		}
		got[p.Data.ID] = true
	}
	for _, id := range ids {
		if !got[id] {
			t.Fatalf("message %d never made it into a delivery row", id)
		}
	}
}

func countDeliveries(t *testing.T, st *store.Store, endpointID int64) int {
	t.Helper()
	log, err := st.ListWebhookDeliveries(endpointID, 200)
	if err != nil {
		t.Fatal(err)
	}
	return len(log)
}
