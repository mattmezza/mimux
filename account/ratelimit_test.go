// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLimiterWindow(t *testing.T) {
	l := newLimiter(3, time.Hour)
	t0 := time.Now()

	for i := range 3 {
		if !l.allow("a", t0) {
			t.Fatalf("attempt %d blocked, budget is 3", i+1)
		}
	}
	if l.allow("a", t0) {
		t.Fatal("4th attempt allowed")
	}
	if !l.allow("b", t0) {
		t.Fatal("a different key shares the budget")
	}
	// Still blocked just inside the window, free again once it has passed.
	if l.allow("a", t0.Add(59*time.Minute)) {
		t.Error("allowed inside the window")
	}
	if !l.allow("a", t0.Add(61*time.Minute)) {
		t.Error("still blocked after the window")
	}
}

func TestLimiterSweepsStaleKeys(t *testing.T) {
	l := newLimiter(3, time.Minute)
	t0 := time.Now()
	for i := range 5000 {
		l.allow(string(rune(i))+"x", t0)
	}
	l.allow("trigger", t0.Add(time.Hour)) // sweep runs once the map is big
	if len(l.hits) > 4096 {
		t.Errorf("map = %d entries, stale keys never dropped", len(l.hits))
	}
}

func retrieve(t *testing.T, a *app, form, ip string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/retrieve", strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Forwarded-For", ip)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	return w
}

// TestRetrieveIsNotAnOracle: same answer for a customer and a stranger, key
// never rendered, and the fourth attempt in an hour does nothing.
func TestRetrieveIsNotAnOracle(t *testing.T) {
	a, sent := newTestApp(t)
	body := checkoutEvent("evt_r", "subscription", planAnnual)
	if w := post(t, a, body, signed(t, body, time.Now())); w.Code != http.StatusOK {
		t.Fatalf("seed: %d", w.Code)
	}
	*sent = nil

	customer := retrieve(t, a, "email=buyer@example.com&action=licence", "203.0.113.1")
	stranger := retrieve(t, a, "email=nobody@example.com&action=licence", "203.0.113.2")
	if customer.Code != http.StatusOK || stranger.Code != http.StatusOK {
		t.Fatalf("codes = %d, %d", customer.Code, stranger.Code)
	}
	if customer.Body.String() != stranger.Body.String() {
		t.Error("the page distinguishes a customer from a stranger")
	}
	if strings.Contains(customer.Body.String(), keyPrefix+".") {
		t.Error("licence key rendered in the browser")
	}
	if len(*sent) != 1 || (*sent)[0].to != "buyer@example.com" {
		t.Fatalf("emails = %+v, want one to the customer", *sent)
	}
	if !strings.Contains((*sent)[0].body, keyPrefix+".") {
		t.Error("resent email carries no key")
	}

	// Budget is 3/hour per IP; the customer has used one.
	for range 2 {
		retrieve(t, a, "email=buyer@example.com&action=licence", "203.0.113.1")
	}
	before := len(*sent)
	retrieve(t, a, "email=buyer@example.com&action=licence", "203.0.113.1")
	if len(*sent) != before {
		t.Errorf("rate limit did not hold: %d -> %d emails", before, len(*sent))
	}

	// The same address from a fresh IP is still capped by the per-email budget.
	before = len(*sent)
	retrieve(t, a, "email=buyer@example.com&action=licence", "198.51.100.7")
	if len(*sent) != before {
		t.Errorf("per-email limit did not hold: %d -> %d emails", before, len(*sent))
	}
}

// A caller who sets their own X-Forwarded-For must not get a fresh per-IP
// budget. The proxy appends the real peer, so the forged value sits first and
// the trustworthy one last — read the wrong end and the limit is decorative.
func TestClientIPIgnoresForgedForwardedFor(t *testing.T) {
	r := httptest.NewRequest("POST", "/retrieve", nil)
	r.RemoteAddr = "127.0.0.1:12345"

	// What nginx/Caddy actually forward when the client sent "1.2.3.4".
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("forged prefix won: clientIP = %q, want 203.0.113.9", got)
	}

	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("single hop: clientIP = %q, want 203.0.113.9", got)
	}

	r.Header.Del("X-Forwarded-For")
	if got := clientIP(r); got != "127.0.0.1" {
		t.Errorf("no header: clientIP = %q, want 127.0.0.1", got)
	}
}
