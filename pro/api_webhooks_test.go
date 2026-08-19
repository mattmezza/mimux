//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattmezza/mimux/internal/store"
)

// createWebhook posts one endpoint through the API and returns the parsed
// response.
func createWebhook(t *testing.T, ta *testAPI, url string, events ...string) (int64, string) {
	t.Helper()
	rec := ta.req(t, http.MethodPost, "/v1/webhooks", map[string]any{"url": url, "events": events})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create webhook = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Webhook webhookJSON `json:"webhook"`
		Secret  string      `json:"secret"`
		Note    string      `json:"note"`
	}
	decodeBody(t, rec, &out)
	if out.Secret == "" || !strings.Contains(out.Note, "shown once") {
		t.Fatalf("create response does not hand over the secret once: %s", rec.Body.String())
	}
	return out.Webhook.ID, out.Secret
}

// TestWebhookAPICRUD walks the whole endpoint lifecycle over the API, including
// the one-shot secret and the auto-disable reset.
func TestWebhookAPICRUD(t *testing.T) {
	ta := newTestAPI(t)

	id, secret := createWebhook(t, ta, "https://example.test/hook", "message.received", "bogus.event")
	stored, err := ta.st.WebhookEndpointByID(id)
	if err != nil || stored == nil {
		t.Fatalf("endpoint not stored: %v", err)
	}
	if stored.Secret != secret {
		t.Error("the secret handed to the caller is not the one that will sign")
	}
	if stored.Events != "message.received" {
		t.Errorf("events = %q, want the unknown one dropped", stored.Events)
	}

	// Listing never hands the secret back.
	rec := ta.req(t, http.MethodGet, "/v1/webhooks", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("the webhook list leaks the signing secret")
	}
	var list struct {
		Data []webhookJSON `json:"data"`
	}
	decodeBody(t, rec, &list)
	if len(list.Data) != 1 || list.Data[0].URL != "https://example.test/hook" || !list.Data[0].Active {
		t.Fatalf("list = %+v", list.Data)
	}

	// PATCH: url + events, and re-activating clears an auto-disabled stamp.
	if err := ta.st.AutoDisableWebhookEndpoint(id, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec = ta.req(t, http.MethodGet, "/v1/webhooks", nil)
	decodeBody(t, rec, &list)
	if list.Data[0].Active || list.Data[0].AutoDisabledAt == "" {
		t.Fatalf("auto-disabled state is not visible over the API: %+v", list.Data[0])
	}
	path := "/v1/webhooks/" + strconv.FormatInt(id, 10)
	rec = ta.req(t, http.MethodPatch, path, map[string]any{
		"url": "https://example.test/other", "events": []string{"sync.error"}, "active": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	var patched webhookJSON
	decodeBody(t, rec, &patched)
	if !patched.Active || patched.AutoDisabledAt != "" || patched.URL != "https://example.test/other" ||
		len(patched.Events) != 1 || patched.Events[0] != "sync.error" {
		t.Fatalf("patch result = %+v", patched)
	}

	// A URL the engine could never post to is a 400, not a stored row.
	if rec := ta.req(t, http.MethodPatch, path, map[string]any{"url": "ftp://example.test"}); rec.Code != http.StatusBadRequest {
		t.Errorf("patch with a bad url = %d, want 400", rec.Code)
	}

	if rec := ta.req(t, http.MethodDelete, path, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	if gone, _ := ta.st.WebhookEndpointByID(id); gone != nil {
		t.Error("endpoint survived delete")
	}
	if rec := ta.req(t, http.MethodGet, path+"/deliveries", nil); rec.Code != http.StatusNotFound {
		t.Errorf("deliveries of a deleted webhook = %d, want 404", rec.Code)
	}
}

// TestWebhookAPITestAndReplay: the ping is queued and sent, and a replay
// re-sends the same delivery id.
func TestWebhookAPITestAndReplay(t *testing.T) {
	rc := newRecorder(http.StatusOK)
	srv := httptest.NewServer(rc)
	defer srv.Close()

	ta := newTestAPI(t)
	id, secret := createWebhook(t, ta, srv.URL) // subscribed to nothing: ping ignores that
	path := "/v1/webhooks/" + strconv.FormatInt(id, 10)

	rec := ta.req(t, http.MethodPost, path+"/test", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("test = %d: %s", rec.Code, rec.Body.String())
	}
	var queued deliveryJSON
	decodeBody(t, rec, &queued)
	if queued.Event != "ping" || queued.DeliveryID == "" {
		t.Fatalf("test delivery = %+v", queued)
	}

	waitFor(t, rc, 1)
	body, head := rc.last()
	verify(t, secret, head.Get("X-Mimux-Signature"), body)
	if head.Get("X-Mimux-Event") != "ping" {
		t.Errorf("event header = %q", head.Get("X-Mimux-Event"))
	}

	// Replay: same delivery id on the wire, a second attempt in the log.
	rec = ta.req(t, http.MethodPost, path+"/deliveries/"+strconv.FormatInt(queued.ID, 10)+"/replay", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("replay = %d: %s", rec.Code, rec.Body.String())
	}
	waitFor(t, rc, 2)
	if _, head := rc.last(); head.Get("X-Mimux-Delivery-Id") != queued.DeliveryID {
		t.Error("replay changed the delivery id")
	}

	rec = ta.req(t, http.MethodGet, path+"/deliveries", nil)
	var log struct {
		Data []deliveryJSON `json:"data"`
	}
	decodeBody(t, rec, &log)
	if len(log.Data) != 1 || log.Data[0].Status != store.WebhookOK || log.Data[0].Attempts == 0 {
		t.Fatalf("delivery log = %+v", log.Data)
	}

	// A delivery id from nowhere is a 404 rather than someone else's replay.
	if rec := ta.req(t, http.MethodPost, path+"/deliveries/99999/replay", nil); rec.Code != http.StatusNotFound {
		t.Errorf("replay of an unknown delivery = %d, want 404", rec.Code)
	}
}

// TestSearchCompletedFires: a deep-search job that finishes queues (and sends)
// search.completed — the event that exists because that job's answer arrives
// long after its HTTP response did.
func TestSearchCompletedFires(t *testing.T) {
	rc := newRecorder(http.StatusOK)
	srv := httptest.NewServer(rc)
	defer srv.Close()

	ta := newTestAPI(t)
	createWebhook(t, ta, srv.URL, "search.completed")

	rec := ta.req(t, http.MethodPost, "/v1/messages/search", map[string]any{
		"query": "from:ada", "mode": "deep",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deep search = %d: %s", rec.Code, rec.Body.String())
	}
	var job struct {
		ID string `json:"id"`
	}
	decodeBody(t, rec, &job)

	waitFor(t, rc, 1)
	body, head := rc.last()
	if head.Get("X-Mimux-Event") != "search.completed" || !strings.Contains(body, job.ID) {
		t.Fatalf("search.completed payload = %s (headers %v)", body, head)
	}
}

// seedDelivery writes one delivery straight through the store, bypassing the
// engine, so a stats/filter test can pin an exact status and code instead of
// waiting on a real retry ladder.
func seedDelivery(t *testing.T, st *store.Store, epID int64, event, status string, code int) *store.WebhookDelivery {
	t.Helper()
	d := &store.WebhookDelivery{EndpointID: epID, EventType: event, DeliveryID: "seed", Payload: "{}"}
	if err := st.EnqueueWebhookDelivery(d); err != nil {
		t.Fatal(err)
	}
	d.DeliveryID = "seed-" + strconv.FormatInt(d.ID, 10)
	d.Status = status
	d.Attempts = 1
	d.LastStatusCode = code
	d.ResponseBody = "seeded response"
	d.DurationMS = 42
	if status == store.WebhookOK {
		d.DeliveredAt = time.Now().UTC()
	}
	if err := st.SaveWebhookDelivery(d); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestWebhookStats: the numbers the Settings UI shows next to each endpoint —
// success rate over settled deliveries, the failing count, and the last
// delivery — come back on both the listing and any single-webhook response.
func TestWebhookStats(t *testing.T) {
	ta := newTestAPI(t)
	id, _ := createWebhook(t, ta, "https://example.test/hook", "message.received")

	seedDelivery(t, ta.st, id, "message.received", store.WebhookOK, 200)
	seedDelivery(t, ta.st, id, "message.received", store.WebhookOK, 200)
	seedDelivery(t, ta.st, id, "message.received", store.WebhookDead, 500)
	seedDelivery(t, ta.st, id, "message.received", store.WebhookFailed, 503)
	last := seedDelivery(t, ta.st, id, "message.received", store.WebhookPending, 0)

	rec := ta.req(t, http.MethodGet, "/v1/webhooks", nil)
	var list struct {
		Data []webhookJSON `json:"data"`
	}
	decodeBody(t, rec, &list)
	if len(list.Data) != 1 {
		t.Fatalf("list = %+v", list.Data)
	}
	st := list.Data[0].Stats
	if st.Total != 5 {
		t.Errorf("total = %d, want 5", st.Total)
	}
	// 2 ok out of 3 settled (2 ok + 1 dead) = 66%.
	if st.SuccessRate == nil || *st.SuccessRate != 66 {
		t.Errorf("success_rate = %v, want 66", st.SuccessRate)
	}
	if st.Failing != 2 { // 1 failed (retrying) + 1 dead
		t.Errorf("failing = %d, want 2", st.Failing)
	}
	if st.Pending != 1 {
		t.Errorf("pending = %d, want 1", st.Pending)
	}
	if st.LastStatus != store.WebhookPending || st.LastDeliveryAt == "" {
		t.Errorf("last delivery = %+v, want the most recently queued row (%d)", st, last.ID)
	}

	// A patch response carries the same numbers — it is not list-only.
	path := "/v1/webhooks/" + strconv.FormatInt(id, 10)
	rec = ta.req(t, http.MethodPatch, path, map[string]any{"active": true})
	var patched webhookJSON
	decodeBody(t, rec, &patched)
	if patched.Stats.Total != 5 {
		t.Errorf("patch stats.total = %d, want 5", patched.Stats.Total)
	}

	// A fresh endpoint has nothing settled yet: success_rate stays absent
	// rather than lying with a 0%. Decoded into its own variable — reusing
	// list here would leave a stale pointer from the row above, since
	// encoding/json does not zero fields absent from the new JSON.
	id2, _ := createWebhook(t, ta, "https://example.test/other", "message.received")
	rec = ta.req(t, http.MethodGet, "/v1/webhooks", nil)
	var list2 struct {
		Data []webhookJSON `json:"data"`
	}
	decodeBody(t, rec, &list2)
	for _, w := range list2.Data {
		if w.ID == id2 && w.Stats.SuccessRate != nil {
			t.Errorf("fresh endpoint success_rate = %v, want absent", *w.Stats.SuccessRate)
		}
	}
}

// TestWebhookDeliveriesFilterAndPaginate: ?status and ?event narrow the log,
// ?limit and ?cursor page through it, and response_body/duration_ms ride
// along on every row.
func TestWebhookDeliveriesFilterAndPaginate(t *testing.T) {
	ta := newTestAPI(t)
	id, _ := createWebhook(t, ta, "https://example.test/hook", "message.received", "sync.error")
	path := "/v1/webhooks/" + strconv.FormatInt(id, 10) + "/deliveries"

	for i := 0; i < 3; i++ {
		seedDelivery(t, ta.st, id, "message.received", store.WebhookOK, 200)
	}
	seedDelivery(t, ta.st, id, "sync.error", store.WebhookDead, 500)

	// response_body and duration_ms are on the wire.
	rec := ta.req(t, http.MethodGet, path+"?limit=1", nil)
	var page struct {
		Data       []deliveryJSON `json:"data"`
		NextCursor string         `json:"next_cursor"`
	}
	decodeBody(t, rec, &page)
	if len(page.Data) != 1 || page.Data[0].ResponseBody != "seeded response" || page.Data[0].DurationMS != 42 {
		t.Fatalf("page = %+v", page.Data)
	}
	if page.NextCursor == "" {
		t.Fatal("next_cursor is empty with more rows left")
	}

	// Follow the cursor to the end without dropping or repeating a row.
	seen := map[int64]bool{page.Data[0].ID: true}
	cursor := page.NextCursor
	for cursor != "" {
		rec = ta.req(t, http.MethodGet, path+"?limit=1&cursor="+cursor, nil)
		decodeBody(t, rec, &page)
		if len(page.Data) != 1 {
			t.Fatalf("page at cursor %s = %+v", cursor, page.Data)
		}
		if seen[page.Data[0].ID] {
			t.Fatalf("delivery %d seen twice while paging", page.Data[0].ID)
		}
		seen[page.Data[0].ID] = true
		cursor = page.NextCursor
	}
	if len(seen) != 4 {
		t.Fatalf("paged through %d deliveries, want 4", len(seen))
	}

	// status filter.
	rec = ta.req(t, http.MethodGet, path+"?status="+store.WebhookDead, nil)
	decodeBody(t, rec, &page)
	if len(page.Data) != 1 || page.Data[0].Status != store.WebhookDead {
		t.Fatalf("status filter = %+v", page.Data)
	}

	// event filter.
	rec = ta.req(t, http.MethodGet, path+"?event=sync.error", nil)
	decodeBody(t, rec, &page)
	if len(page.Data) != 1 || page.Data[0].Event != "sync.error" {
		t.Fatalf("event filter = %+v", page.Data)
	}

	// A bad limit is a 400, not a silently clamped one.
	if rec := ta.req(t, http.MethodGet, path+"?limit=0", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("limit=0 = %d, want 400", rec.Code)
	}
	if rec := ta.req(t, http.MethodGet, path+"?limit=101", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("limit=101 = %d, want 400", rec.Code)
	}
}

// TestWebhookPauseResume: pause stops nothing from being queued (that is the
// engine's job — see webhooks_test.go), it only flips active; resume flips it
// back and clears an auto-disabled stamp the same way PATCH active:true does.
func TestWebhookPauseResume(t *testing.T) {
	ta := newTestAPI(t)
	id, _ := createWebhook(t, ta, "https://example.test/hook", "message.received")
	path := "/v1/webhooks/" + strconv.FormatInt(id, 10)

	rec := ta.req(t, http.MethodPost, path+"/pause", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause = %d: %s", rec.Code, rec.Body.String())
	}
	var wj webhookJSON
	decodeBody(t, rec, &wj)
	if wj.Active {
		t.Fatal("paused webhook still active")
	}

	if err := ta.st.AutoDisableWebhookEndpoint(id, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec = ta.req(t, http.MethodPost, path+"/resume", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume = %d: %s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &wj)
	if !wj.Active || wj.AutoDisabledAt != "" {
		t.Fatalf("resume = %+v, want active with no auto_disabled_at", wj)
	}

	if rec := ta.req(t, http.MethodPost, "/v1/webhooks/99999/pause", nil); rec.Code != http.StatusNotFound {
		t.Errorf("pause of an unknown webhook = %d, want 404", rec.Code)
	}
}

// TestWebhookSecretSetAndRotate: the same rule the Settings UI enforces (16
// character minimum, or generate one) and the same one-shot visibility as
// creation — the secret is in this response and never again.
func TestWebhookSecretSetAndRotate(t *testing.T) {
	ta := newTestAPI(t)
	id, original := createWebhook(t, ta, "https://example.test/hook", "message.received")
	path := "/v1/webhooks/" + strconv.FormatInt(id, 10) + "/secret"

	// Too short is a 400, and the stored secret is untouched.
	rec := ta.req(t, http.MethodPost, path, map[string]any{"secret": "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short secret = %d: %s", rec.Code, rec.Body.String())
	}
	stored, _ := ta.st.WebhookEndpointByID(id)
	if stored.Secret != original {
		t.Fatal("a rejected secret was still written")
	}

	// Blank generates one, shown once.
	rec = ta.req(t, http.MethodPost, path, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("generate secret = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Webhook webhookJSON `json:"webhook"`
		Secret  string      `json:"secret"`
		Note    string      `json:"note"`
	}
	decodeBody(t, rec, &out)
	if out.Secret == "" || out.Secret == original || !strings.Contains(out.Note, "shown once") {
		t.Fatalf("generated secret response = %+v", out)
	}
	stored, _ = ta.st.WebhookEndpointByID(id)
	if stored.Secret != out.Secret {
		t.Error("the returned secret is not the one that will sign")
	}
	if strings.Contains(rec.Body.String(), original) {
		t.Error("rotate response leaked the previous secret")
	}

	// A caller-supplied secret at the minimum length is taken as given.
	mine := "exactly-16-chars"
	if len(mine) != 16 {
		t.Fatalf("test fixture is %d chars, want 16", len(mine))
	}
	rec = ta.req(t, http.MethodPost, path, map[string]any{"secret": mine})
	decodeBody(t, rec, &out)
	if out.Secret != mine {
		t.Errorf("secret = %q, want the supplied one", out.Secret)
	}

	// The secret never reappears in a listing.
	rec = ta.req(t, http.MethodGet, "/v1/webhooks", nil)
	if strings.Contains(rec.Body.String(), mine) {
		t.Error("the webhook list leaks the signing secret")
	}

	if rec := ta.req(t, http.MethodPost, "/v1/webhooks/99999/secret", map[string]any{}); rec.Code != http.StatusNotFound {
		t.Errorf("secret on an unknown webhook = %d, want 404", rec.Code)
	}
}

// TestWebhookScopeEnforced: every webhook route needs webhooks:manage, and a
// token without it gets a 403 naming the scope.
func TestWebhookScopeEnforced(t *testing.T) {
	ta := newTestAPI(t)
	id, _ := createWebhook(t, ta, "https://example.test/hook", "message.received")
	readOnly := mintToken(t, ta.st, &store.APIToken{Label: "ro", Scopes: "mail:read"})
	path := "/v1/webhooks/" + strconv.FormatInt(id, 10)

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/webhooks"},
		{http.MethodPost, "/v1/webhooks"},
		{http.MethodPatch, path},
		{http.MethodDelete, path},
		{http.MethodPost, path + "/pause"},
		{http.MethodPost, path + "/resume"},
		{http.MethodPost, path + "/secret"},
		{http.MethodGet, path + "/deliveries"},
		{http.MethodPost, path + "/deliveries/1/replay"},
		{http.MethodPost, path + "/test"},
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+readOnly)
		rec := httptest.NewRecorder()
		ta.h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden || errCode(t, rec) != "insufficient_scope" {
			t.Errorf("%s %s with mail:read = %d, want 403 insufficient_scope", c.method, c.path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "https://example.test/hook") {
			t.Errorf("%s %s leaked the endpoint to a token without the scope", c.method, c.path)
		}
	}
}

// waitFor blocks until the receiver has n deliveries, or the test fails. The
// API hands the sending off to a goroutine, so this is the join.
func waitFor(t *testing.T, rc *recorder, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for rc.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("receiver got %d deliveries, want %d", rc.count(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// listen opens the NDJSON stream against a real server (a ResponseRecorder
// cannot be read while it is being written) and returns a decoder over it.
func listen(t *testing.T, ta *testAPI, query string) *json.Decoder {
	t.Helper()
	srv := httptest.NewServer(ta.h)
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/webhooks/listen"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ta.secret)
	res, err := srv.Client().Do(req) //nolint:bodyclose // closed by the cleanup below
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		t.Fatalf("listen = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content-type = %q", ct)
	}
	if res.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("nginx will buffer this stream")
	}
	return json.NewDecoder(res.Body)
}

// A test delivery reaches a live listener, with the same payload — and the same
// signature over it — a real receiver would get.
func TestWebhookListenStreamsEvents(t *testing.T) {
	ta := newTestAPI(t)
	id, _ := createWebhook(t, ta, "https://example.test/hook", "message.received")
	dec := listen(t, ta, "")

	rec := ta.req(t, http.MethodPost, "/v1/webhooks/"+strconv.FormatInt(id, 10)+"/test", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("test = %d: %s", rec.Code, rec.Body.String())
	}

	var ev liveEvent
	if err := dec.Decode(&ev); err != nil {
		t.Fatalf("decoding the stream: %v", err)
	}
	if ev.Type != "event" || ev.Event != "ping" || ev.ID == "" {
		t.Fatalf("streamed line = %+v", ev)
	}
	var body struct{ ID, Event string }
	if err := json.Unmarshal(ev.Payload, &body); err != nil || body.ID != ev.ID || body.Event != "ping" {
		t.Fatalf("payload is not the delivery envelope: %s", ev.Payload)
	}
}

func TestWebhookListenEventsFilter(t *testing.T) {
	ta := newTestAPI(t)
	id, _ := createWebhook(t, ta, "https://example.test/hook")
	dec := listen(t, ta, "?events=message.received,sync.error")

	// A ping is not in the filter, so it must not appear; the keepalive is 30s
	// away, so the only way to prove a negative here is a short read deadline.
	rec := ta.req(t, http.MethodPost, "/v1/webhooks/"+strconv.FormatInt(id, 10)+"/test", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("test = %d", rec.Code)
	}
	got := make(chan liveEvent, 1)
	go func() {
		var ev liveEvent
		if err := dec.Decode(&ev); err == nil {
			got <- ev
		}
	}()
	select {
	case ev := <-got:
		t.Fatalf("the filter let %q through", ev.Event)
	case <-time.After(300 * time.Millisecond):
	}
}
