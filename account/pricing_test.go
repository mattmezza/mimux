// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	stripe "github.com/stripe/stripe-go/v86"
)

// Every plan must be priced in every currency we offer. A missing entry would
// only surface as a failed checkout for the one customer who picked it.
func TestEveryPlanPricedInEveryCurrency(t *testing.T) {
	for _, plan := range []string{planAnnual, planPerpetual} {
		for _, c := range currencies {
			cents, err := amount(plan, c.Code)
			if err != nil {
				t.Errorf("%s in %s: %v", plan, c.Code, err)
				continue
			}
			if cents <= 0 {
				t.Errorf("%s in %s: non-positive amount %d", plan, c.Code, cents)
			}
		}
	}
}

func TestAmountRejectsUnknowns(t *testing.T) {
	if _, err := amount("enterprise", "eur"); err == nil {
		t.Error("unknown plan was priced")
	}
	if _, err := amount(planAnnual, "gbp"); err == nil {
		t.Error("unsold currency was priced")
	}
}

func TestValidCurrency(t *testing.T) {
	for _, c := range currencies {
		if !validCurrency(c.Code) {
			t.Errorf("%s is offered but not valid", c.Code)
		}
	}
	for _, bad := range []string{"gbp", "EUR", "", "eur "} {
		if validCurrency(bad) {
			t.Errorf("%q accepted as a currency", bad)
		}
	}
}

func TestDisplay(t *testing.T) {
	for _, tc := range []struct {
		cents int64
		sym   string
		want  string
	}{
		{4900, "€", "€49"},
		{9900, "$", "$99"},
		{4950, "€", "€49.50"},
		{4905, "€", "€49.05"},
	} {
		if got := display(tc.cents, tc.sym); got != tc.want {
			t.Errorf("display(%d, %q) = %q, want %q", tc.cents, tc.sym, got, tc.want)
		}
	}
}

// The page must price in the currency asked for, and fall back rather than
// error on one we do not sell.
func TestIndexPricesInRequestedCurrency(t *testing.T) {
	a, _ := newTestApp(t)
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"", "€49"},
		{"?currency=eur", "€49"},
		{"?currency=usd", "$49"},
		{"?currency=gbp", "€49"}, // unsold: falls back to the first currency
	} {
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+tc.query, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET /%s: status %d", tc.query, w.Code)
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("GET /%s: page does not show %s", tc.query, tc.want)
		}
	}
}

// Checkout must refuse a currency we never offered rather than silently
// charging euros — the buyer would be billed in a currency they never saw.
func TestCheckoutRejectsUnknownCurrency(t *testing.T) {
	a, _ := newTestApp(t)
	for _, body := range []string{
		"plan=annual&currency=gbp",
		"plan=annual&currency=",
		"plan=annual",
		"plan=nonsense&currency=eur",
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/checkout", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		a.routes().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("POST /checkout %q: status %d, want 400", body, w.Code)
		}
	}
}

// mimux.dev's buy buttons POST here cross-origin carrying nothing but plan and
// currency, so this drives the handler with exactly what they send and reads
// the session Stripe would have been given. Stripe is stubbed with a local
// server: the assertion is on what left this process, which is the only part
// we own — and it catches a price that stopped coming from pricing.json.
func TestCheckoutSendsStripeWhatTheFormPosted(t *testing.T) {
	a, _ := newTestApp(t)
	var got url.Values
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_test","url":"https://checkout.stripe.com/c/pay/cs_test"}`))
	}))
	t.Cleanup(stub.Close)
	stripe.Key = "sk_test_stub"
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend,
		&stripe.BackendConfig{URL: stripe.String(stub.URL)}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, nil); stripe.Key = "" })

	for _, plan := range []string{planAnnual, planPerpetual} {
		for _, c := range currencies {
			body := "plan=" + plan + "&currency=" + c.Code
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/checkout", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "https://mimux.dev") // as the cross-origin form sends it
			a.routes().ServeHTTP(w, r)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("POST /checkout %q: status %d, want 303", body, w.Code)
			}
			cents, err := amount(plan, c.Code)
			if err != nil {
				t.Fatal(err)
			}
			for field, want := range map[string]string{
				"line_items[0][price_data][currency]":    c.Code,
				"line_items[0][price_data][unit_amount]": strconv.FormatInt(cents, 10),
				"metadata[plan]":                         plan,
				"metadata[currency]":                     c.Code,
			} {
				if got.Get(field) != want {
					t.Errorf("POST /checkout %q: %s = %q, want %q", body, field, got.Get(field), want)
				}
			}
		}
	}
}

// The buy forms must submit the currency the page is priced in. Without it the
// customer reads one price and checkout rejects the request (or, if the field
// were ever defaulted, charges another).
func TestBuyFormsCarryTheDisplayedCurrency(t *testing.T) {
	a, _ := newTestApp(t)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?currency=usd", nil))
	body := w.Body.String()
	if n := strings.Count(body, `name="currency" value="usd"`); n != 2 {
		t.Errorf("expected both buy forms to carry usd, found %d", n)
	}
}
