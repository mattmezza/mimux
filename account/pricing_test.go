// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
