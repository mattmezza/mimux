// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/store"
)

func webhookRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/settings/webhooks", func(w http.ResponseWriter, r *http.Request) { s.renderWebhooks(w, r, "") })
	r.Post("/settings/webhooks", s.handleWebhookCreate)
	r.Post("/settings/webhooks/{id}/pause", s.handleWebhookPause)
	r.Post("/settings/webhooks/{id}/enable", s.handleWebhookEnable)
	r.Post("/settings/webhooks/{id}/secret", s.handleWebhookSecret)
	r.Post("/settings/webhooks/{id}/delete", s.handleWebhookDelete)
	r.Get("/settings/webhooks/{id}/deliveries", s.handleWebhookDeliveries)
	r.Post("/settings/webhooks/{id}/deliveries/{did}/replay", s.handleWebhookReplay)
	return r
}

// TestWebhookCreateShowsSecretOnce: the signing secret is rendered by the
// create response and by nothing afterwards, the same deal as an API token —
// even though this one is still readable in the database, because the engine
// has to sign with it.
func TestWebhookCreateShowsSecretOnce(t *testing.T) {
	s := serverWith(t, nil, nil)
	r := webhookRouter(s)

	rec := postAPIToken(t, r, "/settings/webhooks", url.Values{
		"url":    {"https://example.test/hook"},
		"events": {"message.received", "not-an-event"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	eps, err := s.store.ListWebhookEndpoints()
	if err != nil || len(eps) != 1 {
		t.Fatalf("ListWebhookEndpoints = %d, %v", len(eps), err)
	}
	ep := eps[0]
	if ep.Events != "message.received" {
		t.Errorf("events = %q, want the unknown one dropped", ep.Events)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ep.Secret) || !strings.Contains(body, "Shown only once") {
		t.Fatalf("create response does not reveal the secret once: %s", body)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/webhooks", nil))
	if strings.Contains(rec.Body.String(), ep.Secret) {
		t.Error("the secret is shown again on a plain manager render")
	}

	// A URL the engine could never post to is refused with a readable message.
	if rec := postAPIToken(t, r, "/settings/webhooks", url.Values{"url": {"nope"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("junk url = %d, want 400", rec.Code)
	}
	if eps, _ := s.store.ListWebhookEndpoints(); len(eps) != 1 {
		t.Errorf("rejected request still created an endpoint: %d rows", len(eps))
	}
}

// TestWebhookManagerLifecycle: auto-disabled state is visible and recoverable,
// a delivery can be replayed from the log, and delete removes the endpoint.
func TestWebhookManagerLifecycle(t *testing.T) {
	s := serverWith(t, nil, nil)
	r := webhookRouter(s)

	ep := &store.WebhookEndpoint{URL: "https://example.test/hook", Secret: "s", Events: "message.received", Active: true}
	if err := s.store.CreateWebhookEndpoint(ep); err != nil {
		t.Fatal(err)
	}
	d := &store.WebhookDelivery{EndpointID: ep.ID, EventType: "message.received", DeliveryID: "d1", Payload: "{}"}
	if err := s.store.EnqueueWebhookDelivery(d); err != nil {
		t.Fatal(err)
	}
	d.Status, d.Attempts, d.LastStatusCode = store.WebhookDead, 7, 500
	if err := s.store.SaveWebhookDelivery(d); err != nil {
		t.Fatal(err)
	}
	if err := s.store.AutoDisableWebhookEndpoint(ep.ID, d.CreatedAt); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/webhooks", nil))
	body := rec.Body.String()
	for _, want := range []string{"auto-disabled", "message.received", "dead", "HTTP 500", "Resume", "Replay"} {
		if !strings.Contains(body, want) {
			t.Errorf("manager render missing %q", want)
		}
	}

	id := strconv.FormatInt(ep.ID, 10)
	if rec := postAPIToken(t, r, "/settings/webhooks/"+id+"/deliveries/"+strconv.FormatInt(d.ID, 10)+"/replay", nil); rec.Code != http.StatusOK {
		t.Fatalf("replay = %d: %s", rec.Code, rec.Body.String())
	}
	back, _ := s.store.WebhookDeliveryByID(d.ID)
	if back.Status != store.WebhookPending || back.Attempts != 0 {
		t.Errorf("replay did not re-queue the delivery: %+v", back)
	}
	// A delivery id that belongs to another endpoint is a 404, not a replay.
	if rec := postAPIToken(t, r, "/settings/webhooks/9999/deliveries/"+strconv.FormatInt(d.ID, 10)+"/replay", nil); rec.Code != http.StatusNotFound {
		t.Errorf("replay on an unknown endpoint = %d, want 404", rec.Code)
	}

	if rec := postAPIToken(t, r, "/settings/webhooks/"+id+"/enable", nil); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := s.store.WebhookEndpointByID(ep.ID)
	if !got.Active || got.AutoDisabled() {
		t.Errorf("enable did not clear the auto-disabled stamp: %+v", got)
	}

	if rec := postAPIToken(t, r, "/settings/webhooks/"+id+"/delete", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	if eps, _ := s.store.ListWebhookEndpoints(); len(eps) != 0 {
		t.Errorf("endpoint survived delete: %+v", eps)
	}
}

// TestWebhookPauseResumeAndSecret: pausing keeps the endpoint and its queue,
// resuming clears the auto-disabled stamp, and the secret can be rotated —
// generated (shown once) or brought along by the user.
func TestWebhookPauseResumeAndSecret(t *testing.T) {
	s := serverWith(t, nil, nil)
	r := webhookRouter(s)
	ep := &store.WebhookEndpoint{URL: "https://example.test/hook", Secret: "the-old-secret-value", Events: "message.received", Active: true}
	if err := s.store.CreateWebhookEndpoint(ep); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(ep.ID, 10)

	if rec := postAPIToken(t, r, "/settings/webhooks/"+id+"/pause", nil); rec.Code != http.StatusOK {
		t.Fatalf("pause = %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := s.store.WebhookEndpointByID(ep.ID); got.Active {
		t.Error("pause did not deactivate the endpoint")
	}
	if rec := postAPIToken(t, r, "/settings/webhooks/"+id+"/enable", nil); rec.Code != http.StatusOK {
		t.Fatalf("resume = %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := s.store.WebhookEndpointByID(ep.ID); !got.Active {
		t.Error("resume did not reactivate the endpoint")
	}

	// A secret the user typed is stored as given, and not echoed back.
	rec := postAPIToken(t, r, "/settings/webhooks/"+id+"/secret", url.Values{"secret": {"a-brand-new-secret-value"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set secret = %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := s.store.WebhookEndpointByID(ep.ID)
	if got.Secret != "a-brand-new-secret-value" {
		t.Errorf("secret = %q, want the one just set", got.Secret)
	}
	if strings.Contains(rec.Body.String(), got.Secret) {
		t.Error("a secret the user typed is echoed back into the page")
	}

	// Too short is refused rather than stored.
	if rec := postAPIToken(t, r, "/settings/webhooks/"+id+"/secret", url.Values{"secret": {"short"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("short secret = %d, want 400", rec.Code)
	}

	// Blank generates one and shows it exactly once.
	rec = postAPIToken(t, r, "/settings/webhooks/"+id+"/secret", url.Values{"secret": {""}})
	if rec.Code != http.StatusOK {
		t.Fatalf("generate secret = %d: %s", rec.Code, rec.Body.String())
	}
	after, _ := s.store.WebhookEndpointByID(ep.ID)
	if after.Secret == got.Secret {
		t.Error("blank did not generate a new secret")
	}
	if !strings.Contains(rec.Body.String(), after.Secret) {
		t.Error("a generated secret is never shown")
	}
}

// TestWebhookDeliveriesPage: the debugging view shows the request body and the
// receiver's answer, filters, and sends a replay back to where it came from.
func TestWebhookDeliveriesPage(t *testing.T) {
	s := serverWith(t, nil, nil)
	r := webhookRouter(s)
	ep := &store.WebhookEndpoint{URL: "https://example.test/hook", Secret: "s", Events: "message.received", Active: true}
	if err := s.store.CreateWebhookEndpoint(ep); err != nil {
		t.Fatal(err)
	}
	ok := &store.WebhookDelivery{EndpointID: ep.ID, EventType: "message.sent", DeliveryID: "d-ok", Payload: `{"marker":"payload-one"}`}
	bad := &store.WebhookDelivery{EndpointID: ep.ID, EventType: "message.received", DeliveryID: "d-bad", Payload: `{"marker":"payload-two"}`}
	for _, d := range []*store.WebhookDelivery{ok, bad} {
		if err := s.store.EnqueueWebhookDelivery(d); err != nil {
			t.Fatal(err)
		}
	}
	ok.Status, ok.LastStatusCode, ok.DurationMS = store.WebhookOK, 200, 42
	bad.Status, bad.LastStatusCode, bad.ResponseBody = store.WebhookDead, 400, "unknown event type"
	for _, d := range []*store.WebhookDelivery{ok, bad} {
		if err := s.store.SaveWebhookDelivery(d); err != nil {
			t.Fatal(err)
		}
	}

	base := "/settings/webhooks/" + strconv.FormatInt(ep.ID, 10) + "/deliveries"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("deliveries = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"payload-one", "payload-two", "unknown event type", "42 ms", "50% delivered"} {
		if !strings.Contains(body, want) {
			t.Errorf("deliveries page missing %q", want)
		}
	}

	// Filtering is a plain query string, so it is a bookmarkable URL.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+"?status=dead", nil))
	filtered := rec.Body.String()
	if strings.Contains(filtered, "payload-one") || !strings.Contains(filtered, "payload-two") {
		t.Error("status filter did not narrow the log")
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+"?event=message.sent", nil))
	if body := rec.Body.String(); strings.Contains(body, "payload-two") || !strings.Contains(body, "payload-one") {
		t.Error("event filter did not narrow the log")
	}

	// A replay from this screen comes back to this screen.
	rec = postAPIToken(t, r, base+"/"+strconv.FormatInt(bad.ID, 10)+"/replay", url.Values{"back": {base + "?status=dead"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("replay from the deliveries page = %d, want a redirect back", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != base+"?status=dead" {
		t.Errorf("replay redirected to %q", loc)
	}
	// An off-site "back" is ignored rather than followed.
	rec = postAPIToken(t, r, base+"/"+strconv.FormatInt(bad.ID, 10)+"/replay", url.Values{"back": {"https://evil.test/"}})
	if rec.Code != http.StatusOK {
		t.Errorf("off-site back = %d, want the fragment render", rec.Code)
	}
}

// TestSettingsPageHasWebhooks checks the section is wired into the settings
// page itself, including the honest note that the free build never delivers.
func TestSettingsPageHasWebhooks(t *testing.T) {
	s := serverWith(t, nil, nil)
	body := renderSection(t, s, "webhooks")
	for _, want := range []string{
		`id="webhooks"`, `name="url"`, `name="events" value="message.received"`,
		`hx-post="/settings/webhooks"`, "Webhook delivery is part of mimux pro",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}
