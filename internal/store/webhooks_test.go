// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"strconv"
	"testing"
	"time"
)

func newEndpoint(t *testing.T, s *Store, url, events string) *WebhookEndpoint {
	t.Helper()
	ep := &WebhookEndpoint{URL: url, Secret: "s3cret", Events: events, Active: true}
	if err := s.CreateWebhookEndpoint(ep); err != nil {
		t.Fatal(err)
	}
	return ep
}

func TestWebhookEndpointCRUD(t *testing.T) {
	s := open(t)

	if eps, _ := s.ListWebhookEndpoints(); len(eps) != 0 {
		t.Fatalf("fresh db has %d endpoints", len(eps))
	}
	// A URL the delivery engine could never post to is refused at the boundary,
	// not stored and retried forever.
	for _, bad := range []string{"", "  ", "example.com/hook", "file:///etc/passwd", "javascript:alert(1)"} {
		if err := s.CreateWebhookEndpoint(&WebhookEndpoint{URL: bad, Secret: "s"}); err == nil {
			t.Errorf("url %q was accepted", bad)
		}
	}
	if err := s.CreateWebhookEndpoint(&WebhookEndpoint{URL: "https://example.test/h"}); err == nil {
		t.Error("endpoint without a secret was accepted")
	}

	ep := newEndpoint(t, s, "https://example.test/hook", "message.sent message.received nope")
	if ep.ID == 0 || ep.CreatedAt.IsZero() {
		t.Fatalf("insert did not fill ID/CreatedAt: %+v", ep)
	}
	if ep.Events != "message.received message.sent" {
		t.Errorf("events = %q, want them ordered and the unknown one dropped", ep.Events)
	}

	got, err := s.WebhookEndpointByID(ep.ID)
	if err != nil || got == nil {
		t.Fatalf("WebhookEndpointByID = %v, %v", got, err)
	}
	if got.URL != ep.URL || got.Secret != "s3cret" || !got.Active || got.AutoDisabled() {
		t.Errorf("not round-tripped: %+v", got)
	}
	if !got.Wants("message.sent") || got.Wants("sync.error") {
		t.Errorf("Wants wrong on %q", got.Events)
	}
	if missing, _ := s.WebhookEndpointByID(9999); missing != nil {
		t.Error("nonexistent endpoint found")
	}

	// Update: url + events + paused.
	got.URL, got.Events, got.Active = "http://localhost:9000/h", "sync.error", false
	if err := s.UpdateWebhookEndpoint(got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.WebhookEndpointByID(ep.ID)
	if got.URL != "http://localhost:9000/h" || got.Events != "sync.error" || got.Active {
		t.Errorf("update not applied: %+v", got)
	}

	// Auto-disable stamps the reason and switches it off; re-activating clears it.
	at := time.Now().UTC().Truncate(time.Second)
	if err := s.AutoDisableWebhookEndpoint(ep.ID, at); err != nil {
		t.Fatal(err)
	}
	got, _ = s.WebhookEndpointByID(ep.ID)
	if got.Active || !got.AutoDisabled() || !got.AutoDisabledAt.Equal(at) {
		t.Errorf("auto-disable not recorded: %+v", got)
	}
	got.Active, got.AutoDisabledAt = true, time.Time{}
	if err := s.UpdateWebhookEndpoint(got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.WebhookEndpointByID(ep.ID)
	if !got.Active || got.AutoDisabled() {
		t.Errorf("re-activating did not clear the stamp: %+v", got)
	}

	// Delete takes the deliveries with it (ON DELETE CASCADE).
	d := &WebhookDelivery{EndpointID: ep.ID, EventType: "ping", DeliveryID: "abc", Payload: "{}"}
	if err := s.EnqueueWebhookDelivery(d); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWebhookEndpoint(ep.ID); err != nil {
		t.Fatal(err)
	}
	if eps, _ := s.ListWebhookEndpoints(); len(eps) != 0 {
		t.Errorf("endpoint survived delete: %+v", eps)
	}
	if left, _ := s.WebhookDeliveryByID(d.ID); left != nil {
		t.Errorf("delivery survived its endpoint: %+v", left)
	}
}

func TestValidWebhookEvents(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""}, // no fallback: subscribing someone to an event they didn't ask for sends them mail metadata
		{[]string{"nope"}, ""},
		{[]string{"sync.error", "message.received"}, "message.received sync.error"}, // WebhookEvents order
		{[]string{"ping"}, ""},                                                     // ping is test-only, never a subscription
		{[]string{" message.sent ", "message.sent"}, "message.sent"},               // trimmed, deduped
	}
	for _, c := range cases {
		if got := ValidWebhookEvents(c.in); got != c.want {
			t.Errorf("ValidWebhookEvents(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWebhookDeliveryQueue covers the queue the engine actually drives: what is
// due, what an attempt outcome looks like once saved, and replay.
func TestWebhookDeliveryQueue(t *testing.T) {
	s := open(t)
	ep := newEndpoint(t, s, "https://example.test/hook", "message.received")
	now := time.Now().UTC().Truncate(time.Second)

	due := &WebhookDelivery{EndpointID: ep.ID, EventType: "message.received", DeliveryID: "d1", Payload: `{"id":"d1"}`}
	later := &WebhookDelivery{EndpointID: ep.ID, EventType: "message.received", DeliveryID: "d2", Payload: "{}",
		NextAttemptAt: now.Add(time.Hour)}
	for _, d := range []*WebhookDelivery{due, later} {
		if err := s.EnqueueWebhookDelivery(d); err != nil {
			t.Fatal(err)
		}
	}
	if due.Status != WebhookPending || due.NextAttemptAt.IsZero() {
		t.Errorf("enqueue defaults wrong: %+v", due)
	}

	got, err := s.DueWebhookDeliveries(now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != due.ID {
		t.Fatalf("due = %+v, want only the one scheduled for now", got)
	}
	if got[0].Payload != `{"id":"d1"}` {
		t.Errorf("payload not round-tripped: %q", got[0].Payload)
	}

	// A failed attempt with retries left: still due, later.
	d := got[0]
	d.Status, d.Attempts, d.NextAttemptAt = WebhookFailed, 1, now.Add(time.Minute)
	d.LastStatusCode, d.LastError = 500, "boom"
	if err := s.SaveWebhookDelivery(&d); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.DueWebhookDeliveries(now, 10); len(again) != 0 {
		t.Errorf("a delivery scheduled a minute out is due now: %+v", again)
	}
	if again, _ := s.DueWebhookDeliveries(now.Add(2*time.Minute), 10); len(again) != 1 {
		t.Errorf("a failed delivery is not retried: %+v", again)
	}
	back, _ := s.WebhookDeliveryByID(d.ID)
	if back.Attempts != 1 || back.LastStatusCode != 500 || back.LastError != "boom" || !back.DeliveredAt.IsZero() {
		t.Errorf("attempt outcome not stored: %+v", back)
	}

	// Terminal states drop out of the queue for good.
	for _, status := range []string{WebhookOK, WebhookDead} {
		back.Status, back.NextAttemptAt = status, now.Add(-time.Hour)
		if err := s.SaveWebhookDelivery(back); err != nil {
			t.Fatal(err)
		}
		if again, _ := s.DueWebhookDeliveries(now, 10); len(again) != 0 {
			t.Errorf("%s delivery is still due: %+v", status, again)
		}
	}

	// Replay: pending again, now, with a fresh retry budget and the same
	// delivery id, so a receiver can recognise the duplicate.
	if err := s.ReplayWebhookDelivery(back.ID, now); err != nil {
		t.Fatal(err)
	}
	back, _ = s.WebhookDeliveryByID(back.ID)
	if back.Status != WebhookPending || back.Attempts != 0 || back.DeliveryID != "d1" || !back.DeliveredAt.IsZero() {
		t.Errorf("replay did not re-queue: %+v", back)
	}
	if again, _ := s.DueWebhookDeliveries(now, 10); len(again) != 1 {
		t.Errorf("replayed delivery is not due: %+v", again)
	}
}

// TestWebhookDeliveryPrune pins the bounded log: the last 100 per endpoint, and
// only for that endpoint.
func TestWebhookDeliveryPrune(t *testing.T) {
	s := open(t)
	ep := newEndpoint(t, s, "https://example.test/hook", "ping")
	other := newEndpoint(t, s, "https://other.test/hook", "ping")

	if err := s.EnqueueWebhookDelivery(&WebhookDelivery{
		EndpointID: other.ID, EventType: "ping", DeliveryID: "keep", Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	var first int64
	for i := range webhookDeliveryKeep + 5 {
		d := &WebhookDelivery{EndpointID: ep.ID, EventType: "ping",
			DeliveryID: strconv.Itoa(i), Payload: "{}"}
		if err := s.EnqueueWebhookDelivery(d); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = d.ID
		}
	}

	log, err := s.ListWebhookDeliveries(ep.ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != webhookDeliveryKeep {
		t.Fatalf("log = %d rows, want %d", len(log), webhookDeliveryKeep)
	}
	if log[0].DeliveryID != strconv.Itoa(webhookDeliveryKeep+4) {
		t.Errorf("log is not newest-first: %q", log[0].DeliveryID)
	}
	if gone, _ := s.WebhookDeliveryByID(first); gone != nil {
		t.Error("the oldest delivery survived the prune")
	}
	if kept, _ := s.ListWebhookDeliveries(other.ID, 10); len(kept) != 1 {
		t.Errorf("pruning one endpoint touched another: %+v", kept)
	}
}
