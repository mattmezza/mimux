// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import "fmt"

// The price list. Prices are built into each Checkout Session with price_data
// rather than referenced as Stripe Price objects, so there is nothing to create
// in the dashboard and nothing to keep in sync between here and there: this
// table is the only place an amount lives.
//
// Changing a price changes what new checkouts charge, immediately. Existing
// annual subscriptions keep billing at the amount their subscription was
// created with — Stripe stores that on the subscription, not here — so a rise
// applies to new customers only, which is the behaviour you want anyway.
//
// Amounts are in the currency's smallest unit (cents for both). EUR and USD are
// deliberately the same numerals: the point is to charge a round, familiar
// number in each, not to track an exchange rate.
var prices = map[string]map[string]int64{
	planAnnual:    {"eur": 4900, "usd": 4900},
	planPerpetual: {"eur": 9900, "usd": 9900},
}

// currencies is the display order of the picker. A map has no order, so the
// list that drives the UI is spelled out rather than ranged over `prices`.
var currencies = []currency{
	{Code: "eur", Symbol: "€", Label: "EUR"},
	{Code: "usd", Symbol: "$", Label: "USD"},
}

type currency struct {
	Code   string // lowercase ISO-4217, as Stripe wants it
	Symbol string
	Label  string
}

// planName and planDescription are what Stripe shows on the Checkout page and
// on the receipt. With price_data these travel with the session instead of
// living on a Product in the dashboard.
func planName(plan string) string {
	if plan == planAnnual {
		return "mimux pro — annual"
	}
	return "mimux pro — perpetual"
}

func planDescription(plan string) string {
	if plan == planAnnual {
		return "The mimux automation layer: REST API, MCP server and webhooks. Renews yearly; cancel any time."
	}
	return "The mimux automation layer: REST API, MCP server and webhooks. One payment, covers every release up to the current version."
}

// validCurrency reports whether code is one we sell in. Anything else is a
// client sending something we never offered, so it is rejected rather than
// quietly defaulted — a silent fallback would charge a currency the buyer did
// not choose.
func validCurrency(code string) bool {
	for _, c := range currencies {
		if c.Code == code {
			return true
		}
	}
	return false
}

// amount returns the charge for a plan in a currency, in minor units.
func amount(plan, code string) (int64, error) {
	byCurrency, ok := prices[plan]
	if !ok {
		return 0, fmt.Errorf("unknown plan %q", plan)
	}
	cents, ok := byCurrency[code]
	if !ok {
		return 0, fmt.Errorf("plan %q is not sold in %q", plan, code)
	}
	return cents, nil
}

// display renders an amount for the page: "€49", or "€49.50" if a price ever
// stops being whole.
func display(cents int64, symbol string) string {
	if cents%100 == 0 {
		return fmt.Sprintf("%s%d", symbol, cents/100)
	}
	return fmt.Sprintf("%s%d.%02d", symbol, cents/100, cents%100)
}

// priceList renders both plans in the given currency for the template.
func priceList(code string) (map[string]string, error) {
	out := map[string]string{}
	var sym string
	for _, c := range currencies {
		if c.Code == code {
			sym = c.Symbol
		}
	}
	for _, plan := range []string{planAnnual, planPerpetual} {
		cents, err := amount(plan, code)
		if err != nil {
			return nil, err
		}
		out[plan] = display(cents, sym)
	}
	return out, nil
}
