// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
	billingportal "github.com/stripe/stripe-go/v86/billingportal/session"
	checkout "github.com/stripe/stripe-go/v86/checkout/session"
	"github.com/stripe/stripe-go/v86/webhook"
)

// handleCheckout creates a hosted Checkout Session and redirects to it. Card
// details are entered on stripe.com and never reach this service.
func (a *app) handleCheckout(w http.ResponseWriter, r *http.Request) {
	plan := r.PostFormValue("plan")
	var mode string
	switch plan {
	case planAnnual:
		mode = string(stripe.CheckoutSessionModeSubscription)
	case planPerpetual:
		mode = string(stripe.CheckoutSessionModePayment)
	default:
		http.Error(w, "Unknown plan.", http.StatusBadRequest)
		return
	}
	// Rejected rather than defaulted: a request naming a currency we do not
	// sell in is not a request to charge euros, and guessing would take money
	// in a currency the buyer never saw.
	cur := r.PostFormValue("currency")
	if !validCurrency(cur) {
		http.Error(w, "Unknown currency.", http.StatusBadRequest)
		return
	}
	unit, err := amount(plan, cur)
	if err != nil {
		http.Error(w, "Unknown plan.", http.StatusBadRequest)
		return
	}

	// price_data builds the price into the session, so there are no Price or
	// Product objects to create in the dashboard and nothing to fall out of step
	// with pricing.go. Stripe still records what was charged on the resulting
	// subscription, so renewals bill the amount the customer originally agreed
	// to even if this table changes later.
	item := &stripe.CheckoutSessionLineItemParams{
		Quantity: stripe.Int64(1),
		PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
			Currency:   stripe.String(cur),
			UnitAmount: stripe.Int64(unit),
			ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
				Name:        stripe.String(planName(plan)),
				Description: stripe.String(planDescription(plan)),
			},
		},
	}
	if plan == planAnnual {
		item.PriceData.Recurring = &stripe.CheckoutSessionLineItemPriceDataRecurringParams{
			Interval: stripe.String("year"),
		}
	}

	params := &stripe.CheckoutSessionParams{
		Mode:       stripe.String(mode),
		LineItems:  []*stripe.CheckoutSessionLineItemParams{item},
		SuccessURL: stripe.String(a.cfg.baseURL + "/success"),
		CancelURL:  stripe.String(a.cfg.baseURL + "/cancel"),
	}
	// A customer record for one-off purchases too, so the perpetual buyer has
	// something to open the billing portal against.
	if mode == string(stripe.CheckoutSessionModePayment) {
		params.CustomerCreation = stripe.String("always")
	}
	// plan is what the webhook reads back to decide which licence to sign; it
	// has never come from the price id, so inlining prices changes nothing there.
	params.Metadata = map[string]string{"plan": plan, "currency": cur}

	sess, err := checkout.New(params)
	if err != nil {
		slog.Error("checkout: create session", "plan", plan, "err", err)
		http.Error(w, "Couldn't start checkout. Try again in a minute.", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, sess.URL, http.StatusSeeOther)
}

// checkoutSession is the slice of the Stripe object we act on. Hand-rolled
// rather than stripe.CheckoutSession so the handler doesn't care which API
// version rendered the event.
type checkoutSession struct {
	ID              string `json:"id"`
	Mode            string `json:"mode"`
	Customer        string `json:"customer"`
	CustomerEmail   string `json:"customer_email"`
	CustomerDetails struct {
		Email string `json:"email"`
	} `json:"customer_details"`
	Subscription string            `json:"subscription"`
	Metadata     map[string]string `json:"metadata"`
}

func (s checkoutSession) email() string {
	if s.CustomerDetails.Email != "" {
		return s.CustomerDetails.Email
	}
	return s.CustomerEmail
}

func (s checkoutSession) plan() string {
	if p := s.Metadata["plan"]; p == planAnnual || p == planPerpetual {
		return p
	}
	if s.Mode == string(stripe.CheckoutSessionModeSubscription) {
		return planAnnual
	}
	return planPerpetual
}

func (a *app) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	// Signature and timestamp are both checked here: an unsigned, tampered or
	// stale (replayed hours later) delivery is rejected before anything is read
	// out of the payload.
	//
	// ponytail: API version mismatches are ignored because we only touch fields
	// that have been stable for years (id, customer, subscription, email). Drop
	// the option if this ever starts deserialising whole Stripe objects.
	event, err := webhook.ConstructEventWithOptions(body, r.Header.Get("Stripe-Signature"), a.cfg.webhookSecret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		slog.Warn("webhook: rejected", "err", err)
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}

	switch event.Type {
	case "checkout.session.completed":
		if err := a.issueFromEvent(event.ID, event.Data.Raw); err != nil {
			slog.Error("webhook: issue licence", "event", event.ID, "err", err)
			// 500 so Stripe retries: a failure here means a paying customer has
			// no key.
			http.Error(w, "issue failed", http.StatusInternalServerError)
			return
		}
	case "invoice.payment_succeeded":
		if err := a.renewFromEvent(event.ID, event.Data.Raw); err != nil {
			slog.Error("webhook: renew licence", "event", event.ID, "err", err)
			http.Error(w, "renew failed", http.StatusInternalServerError)
			return
		}
	case "customer.subscription.deleted", "invoice.payment_failed":
		var obj struct {
			ID       string `json:"id"`
			Object   string `json:"object"`
			Customer string `json:"customer"`
		}
		if err := json.Unmarshal(event.Data.Raw, &obj); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		sub := ""
		if obj.Object == "subscription" {
			sub = obj.ID
		}
		if err := lapse(a.db, obj.Customer, sub); err != nil {
			slog.Error("webhook: lapse", "event", event.ID, "err", err)
			http.Error(w, "lapse failed", http.StatusInternalServerError)
			return
		}
		slog.Info("licence lapsed", "customer", obj.Customer, "event", event.Type)
	}
	w.WriteHeader(http.StatusOK)
}

// issueFromEvent signs and stores a licence, then emails it. Claiming the event
// id and inserting the licence happen in one transaction, so a redelivery of
// the same event finds the id taken and issues nothing.
//
// The email is handed to the background sender after the commit and never
// waited on: the licence exists at that point, and Stripe must get its 200
// before it decides we timed out. Returning an error would make it redeliver an
// event that is already claimed, which would resend nothing anyway.
func (a *app) issueFromEvent(eventID string, raw []byte) error {
	var sess checkoutSession
	if err := json.Unmarshal(raw, &sess); err != nil {
		return fmt.Errorf("parse session: %w", err)
	}
	email := sess.email()
	if !validEmail(email) {
		return fmt.Errorf("session %s: no usable customer email", sess.ID)
	}
	plan := sess.plan()
	p := newLicence(email, plan, a.cfg.currentVersion, time.Now())
	key, err := signLicence(a.key, p)
	if err != nil {
		return err
	}

	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	first, err := claimEvent(tx, eventID)
	if err != nil {
		return err
	}
	if !first {
		slog.Info("webhook: duplicate delivery ignored", "event", eventID)
		return nil
	}
	err = insertLicence(tx, licence{
		ID: p.ID, Email: email, Plan: plan, Key: key,
		CustomerID: sess.Customer, Subscription: sess.Subscription,
		CreatedAt: p.IssuedAt,
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	slog.Info("licence issued", "id", p.ID, "plan", plan)

	a.dispatch(p.ID, email, "Your mimux pro licence key", licenceEmail(p, key))
	return nil
}

// invoiceEvent is the slice of an invoice a renewal needs. The subscription id
// moved from the top level into `parent` and the period lives on the line items
// rather than the invoice, so both spellings are read and the widest line period
// wins.
type invoiceEvent struct {
	Subscription string `json:"subscription"`
	Parent       struct {
		SubscriptionDetails struct {
			Subscription string `json:"subscription"`
		} `json:"subscription_details"`
	} `json:"parent"`
	PeriodEnd int64 `json:"period_end"`
	Lines     struct {
		Data []struct {
			Period struct {
				End int64 `json:"end"`
			} `json:"period"`
		} `json:"data"`
	} `json:"lines"`
}

func (inv invoiceEvent) subscriptionID() string {
	if inv.Subscription != "" {
		return inv.Subscription
	}
	return inv.Parent.SubscriptionDetails.Subscription
}

// periodEnd is what the customer has now paid through. Falling back to a year
// out is the safe direction to be wrong in: an invoice was paid, so the licence
// must cover something.
func (inv invoiceEvent) periodEnd(now time.Time) int64 {
	var end int64
	for _, l := range inv.Lines.Data {
		if l.Period.End > end {
			end = l.Period.End
		}
	}
	if end == 0 {
		end = inv.PeriodEnd
	}
	if end == 0 {
		end = now.AddDate(1, 0, 0).Unix()
	}
	return end
}

// renewFromEvent re-issues an annual licence for the period the customer has
// just paid for. Without it an annual subscriber's key expires twelve months
// after purchase while their card keeps being charged.
//
// The event id is claimed in the same transaction as the update, so a
// redelivery renews nothing and sends no second email. Two cases claim the id
// and stop there:
//
//   - No licence for the subscription. The first invoice of a new subscription
//     can arrive before checkout.session.completed; that handler owns first
//     issuance.
//   - The period is not later than what the current key already covers. This is
//     the same first-invoice race seen from the other side, and it needs no
//     assumption about which event Stripe delivers first.
func (a *app) renewFromEvent(eventID string, raw []byte) error {
	var inv invoiceEvent
	if err := json.Unmarshal(raw, &inv); err != nil {
		return fmt.Errorf("parse invoice: %w", err)
	}
	now := time.Now()

	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	first, err := claimEvent(tx, eventID)
	if err != nil {
		return err
	}
	if !first {
		slog.Info("webhook: duplicate delivery ignored", "event", eventID)
		return nil
	}

	l, found, err := licenceBySubscription(tx, inv.subscriptionID())
	if err != nil {
		return err
	}
	if !found {
		slog.Info("invoice paid for an unknown subscription; checkout will issue",
			"event", eventID, "subscription", inv.subscriptionID())
		return tx.Commit()
	}
	current, err := parseLicenceKey(l.Key)
	if err != nil {
		return fmt.Errorf("licence %s: %w", l.ID, err)
	}
	end := inv.periodEnd(now)
	if current.ExpiresAt == nil || end <= *current.ExpiresAt {
		// No re-sign and no email, but the customer has paid, so they are not
		// lapsed — this is how a licence that failed a charge and then cleared
		// it on the same period gets its mark removed.
		if err := activateLicence(tx, l.ID); err != nil {
			return err
		}
		slog.Info("invoice adds no time to the licence; marked active only", "licence", l.ID)
		return tx.Commit()
	}

	p := newLicence(l.Email, l.Plan, a.cfg.currentVersion, now)
	p.ID = l.ID
	p.ExpiresAt = &end
	key, err := signLicence(a.key, p)
	if err != nil {
		return err
	}
	if err := renewLicence(tx, p.ID, key, p.IssuedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	slog.Info("licence renewed", "id", p.ID, "until", end)

	a.dispatch(p.ID, l.Email, "Your renewed mimux pro licence key", renewalEmail(p, key))
	return nil
}

// portalURL creates a Stripe billing portal session for a customer. Used for
// cancellation and card updates; we never touch either ourselves.
func (a *app) portalURL(customerID string) (string, error) {
	s, err := billingportal.New(&stripe.BillingPortalSessionParams{
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(a.cfg.baseURL + "/"),
	})
	if err != nil {
		return "", err
	}
	return s.URL, nil
}
