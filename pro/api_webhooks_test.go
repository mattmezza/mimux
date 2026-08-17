//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
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
