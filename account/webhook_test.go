// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v86/webhook"
)

const testWebhookSecret = "whsec_test_not_a_real_secret"

// TestMain kills the retry backoff: the tests exercise the retry path, they do
// not need to sit through it.
func TestMain(m *testing.M) {
	mailRetryDelay = 0
	os.Exit(m.Run())
}

type sentMail struct{ to, subject, body string }

// mailbox is what the tests read instead of a bare slice — sends now land from
// a background goroutine, so the append needs a lock of its own.
type mailbox struct {
	mu   sync.Mutex
	sent []sentMail
}

func (b *mailbox) add(m sentMail) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, m)
}

func (b *mailbox) all() []sentMail {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sentMail(nil), b.sent...)
}

func (b *mailbox) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = nil
}

func (b *mailbox) len() int { return len(b.all()) }

func newTestApp(t *testing.T) (*app, *mailbox) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openDB(t.TempDir() + "/account.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var box mailbox
	a := &app{
		cfg:        config{webhookSecret: testWebhookSecret, currentVersion: "v0.19", baseURL: "https://account.test"},
		db:         db,
		key:        priv,
		send:       func(to, subject, body string) error { box.add(sentMail{to, subject, body}); return nil },
		ipLimit:    newLimiter(3, time.Hour),
		emailLimit: newLimiter(3, time.Hour),
	}
	if err := a.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	// No goroutine outlives its test: every send is joined before cleanup.
	t.Cleanup(func() { a.mail.Wait() })
	return a, &box
}

// serve runs one request and then waits for the mail it set off, so assertions
// on the mailbox are not racing the background sender. No sleeping: the same
// WaitGroup shutdown drains on.
func serve(a *app, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	a.mail.Wait()
	return w
}

// TestPagesRender walks every GET page: parseTemplates catches syntax errors,
// only executing them catches a field that isn't there.
func TestPagesRender(t *testing.T) {
	a, _ := newTestApp(t)
	for _, path := range []string{"/", "/success", "/cancel", "/retrieve", "/support", "/healthz"} {
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status %d", path, w.Code)
		}
		if w.Body.Len() == 0 || strings.Contains(w.Body.String(), "<no value>") {
			t.Errorf("GET %s: bad render:\n%s", path, w.Body)
		}
	}
}

func post(t *testing.T, a *app, body, sig string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/stripe/webhook", strings.NewReader(body))
	if sig != "" {
		r.Header.Set("Stripe-Signature", sig)
	}
	return serve(a, r)
}

func signed(t *testing.T, body string, at time.Time) string {
	t.Helper()
	return webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: []byte(body), Secret: testWebhookSecret, Timestamp: at,
	}).Header
}

func checkoutEvent(id, mode, plan string) string {
	return `{"id":"` + id + `","object":"event","api_version":"2025-01-01.clover",
	  "type":"checkout.session.completed","data":{"object":{
	    "id":"cs_test_1","object":"checkout.session","mode":"` + mode + `",
	    "customer":"cus_123","subscription":"sub_123",
	    "customer_details":{"email":"buyer@example.com"},
	    "metadata":{"plan":"` + plan + `"}}}}`
}

// invoiceEventJSON uses the modern shape: subscription under parent, period on
// the line item, nothing at the top level.
func invoiceEventJSON(id, typ, subID string, periodEnd int64) string {
	return fmt.Sprintf(`{"id":%q,"object":"event","api_version":"2025-01-01.clover",
	  "type":%q,"data":{"object":{
	    "id":"in_1","object":"invoice","customer":"cus_123",
	    "parent":{"subscription_details":{"subscription":%q}},
	    "lines":{"data":[{"period":{"end":%d}}]}}}}`, id, typ, subID, periodEnd)
}

// storedLicence reads back the single licence row plus the payload of the key
// currently on it.
func storedLicence(t *testing.T, a *app) (licence, licencePayload) {
	t.Helper()
	var l licence
	err := a.db.QueryRow(`SELECT id, key, status, created_at FROM licences`).
		Scan(&l.ID, &l.Key, &l.Status, &l.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	p, err := parseLicenceKey(l.Key)
	if err != nil {
		t.Fatalf("stored key does not parse: %v", err)
	}
	if p.ExpiresAt == nil {
		t.Fatal("stored annual licence has no exp")
	}
	return l, p
}

// seedAnnual runs a checkout event through the handler and returns the licence
// it issued, with the mail recorder emptied.
func seedAnnual(t *testing.T, a *app, sent *mailbox) (licence, licencePayload) {
	t.Helper()
	body := checkoutEvent("evt_seed", "subscription", planAnnual)
	if w := post(t, a, body, signed(t, body, time.Now())); w.Code != http.StatusOK {
		t.Fatalf("seed checkout: %d %s", w.Code, w.Body)
	}
	sent.reset()
	return storedLicence(t, a)
}

// TestRenewalExtendsLicence is the one that stops annual subscribers losing
// their key at month twelve while their card keeps being charged.
func TestRenewalExtendsLicence(t *testing.T) {
	a, sent := newTestApp(t)
	before, beforeKey := seedAnnual(t, a, sent)

	next := *beforeKey.ExpiresAt + 365*24*3600
	body := invoiceEventJSON("evt_renew", "invoice.payment_succeeded", "sub_123", next)
	sig := signed(t, body, time.Now())

	// Delivered twice: the second must change nothing.
	for i := range 2 {
		if w := post(t, a, body, sig); w.Code != http.StatusOK {
			t.Fatalf("delivery %d: %d %s", i+1, w.Code, w.Body)
		}
	}

	if n := countLicences(t, a); n != 1 {
		t.Fatalf("licence rows = %d, want 1 renewed in place", n)
	}
	after, afterKey := storedLicence(t, a)
	if after.ID != before.ID {
		t.Errorf("licence id changed: %s -> %s", before.ID, after.ID)
	}
	if *afterKey.ExpiresAt != next {
		t.Errorf("exp = %d, want %d", *afterKey.ExpiresAt, next)
	}
	if after.Key == before.Key {
		t.Error("key was not re-signed")
	}
	if afterKey.Email != "buyer@example.com" || afterKey.Plan != planAnnual {
		t.Errorf("renewed payload = %+v", afterKey)
	}
	if sent.len() != 1 {
		t.Fatalf("emails = %d, want exactly 1 despite the redelivery", sent.len())
	}
	m := sent.all()[0]
	if !strings.Contains(m.body, after.Key) {
		t.Error("renewal email does not carry the new key")
	}
	if !strings.Contains(m.body, "replaces the previous one") {
		t.Errorf("renewal email does not say it replaces the old key:\n%s", m.body)
	}
}

// TestFirstInvoiceDoesNotDoubleIssue: a new subscription's first invoice covers
// the period the checkout licence already covers, whichever event lands first.
func TestFirstInvoiceDoesNotDoubleIssue(t *testing.T) {
	a, sent := newTestApp(t)
	before, beforeKey := seedAnnual(t, a, sent)

	// Same period end the checkout licence already has, and one a second short.
	for i, end := range []int64{*beforeKey.ExpiresAt, *beforeKey.ExpiresAt - 1} {
		body := invoiceEventJSON(fmt.Sprintf("evt_first_%d", i), "invoice.payment_succeeded", "sub_123", end)
		if w := post(t, a, body, signed(t, body, time.Now())); w.Code != http.StatusOK {
			t.Fatalf("end %d: %d %s", end, w.Code, w.Body)
		}
	}

	// An invoice for a subscription we have never issued against: checkout owns
	// first issuance, this must be a no-op rather than an error.
	body := invoiceEventJSON("evt_unknown", "invoice.payment_succeeded", "sub_elsewhere", time.Now().Unix()+99999)
	if w := post(t, a, body, signed(t, body, time.Now())); w.Code != http.StatusOK {
		t.Fatalf("unknown subscription: %d %s", w.Code, w.Body)
	}

	if n := countLicences(t, a); n != 1 {
		t.Errorf("licence rows = %d, want 1", n)
	}
	after, _ := storedLicence(t, a)
	if after.Key != before.Key {
		t.Error("key was re-issued for an invoice that adds no time")
	}
	if sent.len() != 0 {
		t.Errorf("emails = %d, want none", sent.len())
	}
}

// TestFailedThenRecoveredPayment: a card that fails and is then fixed must end
// up active again, with a key covering the period that was finally paid for.
func TestFailedThenRecoveredPayment(t *testing.T) {
	a, sent := newTestApp(t)
	_, beforeKey := seedAnnual(t, a, sent)
	next := *beforeKey.ExpiresAt + 365*24*3600

	failed := invoiceEventJSON("evt_failed", "invoice.payment_failed", "sub_123", next)
	if w := post(t, a, failed, signed(t, failed, time.Now())); w.Code != http.StatusOK {
		t.Fatalf("failed: %d", w.Code)
	}
	if l, _ := storedLicence(t, a); l.Status != "lapsed" {
		t.Fatalf("status after failure = %q, want lapsed", l.Status)
	}

	ok := invoiceEventJSON("evt_recovered", "invoice.payment_succeeded", "sub_123", next)
	if w := post(t, a, ok, signed(t, ok, time.Now())); w.Code != http.StatusOK {
		t.Fatalf("recovered: %d", w.Code)
	}
	l, p := storedLicence(t, a)
	if l.Status != "active" {
		t.Errorf("status after recovery = %q, want active", l.Status)
	}
	if *p.ExpiresAt != next {
		t.Errorf("exp = %d, want %d", *p.ExpiresAt, next)
	}
	if sent.len() != 1 {
		t.Errorf("emails = %d, want 1 (the renewed key)", sent.len())
	}
}

// TestRecoveredPaymentSamePeriodClearsLapse: the charge that failed is retried
// and clears against the same period, so nothing is re-signed — but the
// customer has paid and must not be left marked lapsed.
func TestRecoveredPaymentSamePeriodClearsLapse(t *testing.T) {
	a, sent := newTestApp(t)
	before, beforeKey := seedAnnual(t, a, sent)
	end := *beforeKey.ExpiresAt // same period: adds no time

	failed := invoiceEventJSON("evt_failed", "invoice.payment_failed", "sub_123", end)
	if w := post(t, a, failed, signed(t, failed, time.Now())); w.Code != http.StatusOK {
		t.Fatalf("failed: %d", w.Code)
	}
	if l, _ := storedLicence(t, a); l.Status != "lapsed" {
		t.Fatalf("status after failure = %q, want lapsed", l.Status)
	}

	ok := invoiceEventJSON("evt_retry_ok", "invoice.payment_succeeded", "sub_123", end)
	if w := post(t, a, ok, signed(t, ok, time.Now())); w.Code != http.StatusOK {
		t.Fatalf("retry: %d", w.Code)
	}

	after, afterKey := storedLicence(t, a)
	if after.Status != "active" {
		t.Errorf("status = %q, want active", after.Status)
	}
	if after.Key != before.Key {
		t.Error("key was re-issued for an invoice that adds no time")
	}
	if *afterKey.ExpiresAt != end {
		t.Errorf("exp = %d, want %d unchanged", *afterKey.ExpiresAt, end)
	}
	if sent.len() != 0 {
		t.Errorf("emails = %d, want none", sent.len())
	}
}

// TestInvoicePeriodFallbacks covers the older invoice shape and an invoice with
// no period at all — a paid invoice must always buy some time.
func TestInvoicePeriodFallbacks(t *testing.T) {
	now := time.Now()
	var legacy invoiceEvent
	err := json.Unmarshal([]byte(`{"subscription":"sub_legacy","period_end":123}`), &legacy)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.subscriptionID() != "sub_legacy" {
		t.Errorf("top-level subscription not read: %q", legacy.subscriptionID())
	}
	if got := legacy.periodEnd(now); got != 123 {
		t.Errorf("period_end fallback = %d, want 123", got)
	}
	var empty invoiceEvent
	if got := empty.periodEnd(now); got != now.AddDate(1, 0, 0).Unix() {
		t.Errorf("bare invoice = %d, want a year out", got)
	}
}

func countLicences(t *testing.T, a *app) int {
	t.Helper()
	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM licences`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestWebhookIssuesOnceOnRedelivery is the one that matters: Stripe retries, and
// a retry must not mint a second licence or send a second email.
func TestWebhookIssuesOnceOnRedelivery(t *testing.T) {
	a, sent := newTestApp(t)
	body := checkoutEvent("evt_1", "subscription", planAnnual)
	sig := signed(t, body, time.Now())

	for i := range 2 {
		if w := post(t, a, body, sig); w.Code != http.StatusOK {
			t.Fatalf("delivery %d: status %d, body %s", i+1, w.Code, w.Body)
		}
	}

	if n := countLicences(t, a); n != 1 {
		t.Fatalf("licences issued = %d, want 1", n)
	}
	if sent.len() != 1 {
		t.Fatalf("emails sent = %d, want 1", sent.len())
	}
	m := sent.all()[0]
	if m.to != "buyer@example.com" {
		t.Errorf("recipient = %q", m.to)
	}
	if !strings.Contains(m.body, keyPrefix+".") {
		t.Errorf("email carries no licence key:\n%s", m.body)
	}

	var plan, status, sub string
	err := a.db.QueryRow(`SELECT plan, status, stripe_subscription_id FROM licences`).Scan(&plan, &status, &sub)
	if err != nil {
		t.Fatal(err)
	}
	if plan != planAnnual || status != "active" || sub != "sub_123" {
		t.Errorf("row = %s/%s/%s", plan, status, sub)
	}
}

func TestWebhookRejectsBadSignatures(t *testing.T) {
	body := checkoutEvent("evt_2", "payment", planPerpetual)
	cases := map[string]string{
		"unsigned":     "",
		"garbage":      "t=1,v1=deadbeef",
		"wrong secret": webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: []byte(body), Secret: "whsec_someone_else"}).Header,
		"stale":        signed(t, body, time.Now().Add(-24*time.Hour)),
	}
	for name, sig := range cases {
		a, sent := newTestApp(t)
		if w := post(t, a, body, sig); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, w.Code)
		}
		if n := countLicences(t, a); n != 0 {
			t.Errorf("%s: issued %d licences", name, n)
		}
		if sent.len() != 0 {
			t.Errorf("%s: sent %d emails", name, sent.len())
		}
	}

	// A body signed correctly but not shaped like an event must not slip past.
	a, _ := newTestApp(t)
	junk := `{"hello":"world"}`
	if w := post(t, a, junk, signed(t, junk, time.Now())); w.Code != http.StatusBadRequest {
		t.Errorf("non-event payload: status = %d, want 400", w.Code)
	}
}

func TestWebhookLapsesOnSubscriptionDeleted(t *testing.T) {
	a, _ := newTestApp(t)
	body := checkoutEvent("evt_3", "subscription", planAnnual)
	if w := post(t, a, body, signed(t, body, time.Now())); w.Code != http.StatusOK {
		t.Fatalf("issue: %d", w.Code)
	}

	del := `{"id":"evt_4","object":"event","api_version":"2025-01-01.clover",
	  "type":"customer.subscription.deleted","data":{"object":{
	    "id":"sub_123","object":"subscription","customer":"cus_123"}}}`
	if w := post(t, a, del, signed(t, del, time.Now())); w.Code != http.StatusOK {
		t.Fatalf("delete: %d", w.Code)
	}

	var status string
	if err := a.db.QueryRow(`SELECT status FROM licences`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "lapsed" {
		t.Errorf("status = %q, want lapsed", status)
	}
	// Record keeping only: the key itself is untouched and still verifies
	// offline until it expires on its own terms.
	var key string
	if err := a.db.QueryRow(`SELECT key FROM licences`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, keyPrefix+".") {
		t.Errorf("key mangled: %q", key)
	}
}

// TestEmailAddressGuard covers the header-injection boundary: /retrieve takes an
// address straight from a form and hands it to the SMTP layer.
func TestEmailAddressGuard(t *testing.T) {
	bad := []string{"", "not-an-address", "a@b.co\r\nBcc: victim@example.com", "a@b.co\nSubject: x", strings.Repeat("a", 250) + "@b.co"}
	for _, s := range bad {
		if validEmail(s) {
			t.Errorf("accepted %q", s)
		}
		if _, err := buildMessage("from@mimux.dev", s, "subject", "body"); err == nil {
			t.Errorf("buildMessage accepted %q", s)
		}
	}
	if !validEmail("buyer@example.com") {
		t.Error("rejected a plain address")
	}
	if _, err := buildMessage("from@mimux.dev", "buyer@example.com", "sub\r\nject", "body"); err == nil {
		t.Error("buildMessage accepted a subject with CRLF")
	}
}
