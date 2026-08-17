//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/store"
)

// deliveryPage is how many log rows GET /webhooks/{id}/deliveries returns. The
// store keeps 100 per endpoint, so this is "all of them" — the log is bounded
// at the write end, which is why there is no cursor here.
const deliveryPage = 100

type webhookJSON struct {
	ID             int64    `json:"id"`
	URL            string   `json:"url"`
	Events         []string `json:"events"`
	Active         bool     `json:"active"`
	AutoDisabledAt string   `json:"auto_disabled_at,omitempty"`
	CreatedAt      string   `json:"created_at"`
}

func toWebhookJSON(ep store.WebhookEndpoint) webhookJSON {
	out := webhookJSON{
		ID: ep.ID, URL: ep.URL, Events: ep.EventList(), Active: ep.Active,
		CreatedAt: ep.CreatedAt.UTC().Format(time.RFC3339),
	}
	if out.Events == nil {
		out.Events = []string{}
	}
	if ep.AutoDisabled() {
		out.AutoDisabledAt = ep.AutoDisabledAt.UTC().Format(time.RFC3339)
	}
	return out
}

type deliveryJSON struct {
	ID             int64  `json:"id"`
	DeliveryID     string `json:"delivery_id"`
	Event          string `json:"event"`
	Status         string `json:"status"`
	Attempts       int    `json:"attempts"`
	LastStatusCode int    `json:"last_status_code,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	NextAttemptAt  string `json:"next_attempt_at,omitempty"`
	CreatedAt      string `json:"created_at"`
	DeliveredAt    string `json:"delivered_at,omitempty"`
}

func toDeliveryJSON(d store.WebhookDelivery) deliveryJSON {
	out := deliveryJSON{
		ID: d.ID, DeliveryID: d.DeliveryID, Event: d.EventType, Status: d.Status,
		Attempts: d.Attempts, LastStatusCode: d.LastStatusCode, LastError: d.LastError,
		CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
	}
	if d.Status == store.WebhookPending || d.Status == store.WebhookFailed {
		out.NextAttemptAt = d.NextAttemptAt.UTC().Format(time.RFC3339)
	}
	if !d.DeliveredAt.IsZero() {
		out.DeliveredAt = d.DeliveredAt.UTC().Format(time.RFC3339)
	}
	return out
}

// endpointOr404 loads the endpoint named by the {id} path param.
func (a *api) endpointOr404(w http.ResponseWriter, r *http.Request) *store.WebhookEndpoint {
	id, ok := pathID(w, r)
	if !ok {
		return nil
	}
	ep, err := a.store.WebhookEndpointByID(id)
	if err != nil || ep == nil {
		apiError(w, http.StatusNotFound, "not_found", "No webhook with id "+strconv.FormatInt(id, 10)+".")
		return nil
	}
	return ep
}

func (a *api) handleListWebhooks(w http.ResponseWriter, _ *http.Request) {
	eps, err := a.store.ListWebhookEndpoints()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't list webhooks.")
		return
	}
	out := make([]webhookJSON, 0, len(eps))
	for _, ep := range eps {
		out = append(out, toWebhookJSON(ep))
	}
	writeList(w, out, "")
}

// handleCreateWebhook registers an endpoint and returns its signing secret —
// the only response that ever contains it. The secret is stored (the engine has
// to read it to sign), but it is never served again: a credential that any
// read-only listing hands back is a credential that leaks through a log.
func (a *api) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
		Active *bool    `json:"active"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ep := &store.WebhookEndpoint{
		URL:    req.URL,
		Secret: auth.NewToken(),
		Events: store.ValidWebhookEvents(req.Events),
		Active: req.Active == nil || *req.Active,
	}
	if err := a.store.CreateWebhookEndpoint(ep); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"webhook": toWebhookJSON(*ep),
		"secret":  ep.Secret,
		"note":    "This secret is shown once. Store it now — it is not returned by any other endpoint. To change it, delete this webhook and create a new one.",
	})
}

// handlePatchWebhook updates url, events and active. Setting active back to
// true clears an auto-disabled stamp: switching it on is how the user says the
// receiver is fixed.
func (a *api) handlePatchWebhook(w http.ResponseWriter, r *http.Request) {
	ep := a.endpointOr404(w, r)
	if ep == nil {
		return
	}
	var req struct {
		URL    *string   `json:"url"`
		Events *[]string `json:"events"`
		Active *bool     `json:"active"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.URL != nil {
		ep.URL = *req.URL
	}
	if req.Events != nil {
		ep.Events = store.ValidWebhookEvents(*req.Events)
	}
	if req.Active != nil {
		ep.Active = *req.Active
		if ep.Active {
			ep.AutoDisabledAt = time.Time{}
		}
	}
	if err := a.store.UpdateWebhookEndpoint(ep); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSON(w, toWebhookJSON(*ep))
}

func (a *api) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	ep := a.endpointOr404(w, r)
	if ep == nil {
		return
	}
	if err := a.store.DeleteWebhookEndpoint(ep.ID); err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't delete the webhook.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) handleWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	ep := a.endpointOr404(w, r)
	if ep == nil {
		return
	}
	dels, err := a.store.ListWebhookDeliveries(ep.ID, deliveryPage)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't read the delivery log.")
		return
	}
	out := make([]deliveryJSON, 0, len(dels))
	for _, d := range dels {
		out = append(out, toDeliveryJSON(d))
	}
	writeList(w, out, "")
}

// handleReplayDelivery re-queues a delivery for immediate sending, keeping its
// delivery id — a receiver that already processed it sees the same
// X-Mimux-Delivery-Id and can ignore the repeat.
func (a *api) handleReplayDelivery(w http.ResponseWriter, r *http.Request) {
	ep := a.endpointOr404(w, r)
	if ep == nil {
		return
	}
	d := a.deliveryOr404(w, r, ep)
	if d == nil {
		return
	}
	if err := a.store.ReplayWebhookDelivery(d.ID, time.Now()); err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't re-queue the delivery.")
		return
	}
	a.sendSoon()
	fresh, _ := a.store.WebhookDeliveryByID(d.ID)
	writeJSONStatus(w, http.StatusAccepted, toDeliveryJSON(*fresh))
}

// handleTestWebhook queues a `ping` delivery to one endpoint, whatever it is
// subscribed to — the point is to prove the URL, the signature check and the
// receiver's parser all work before waiting for real mail.
func (a *api) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	ep := a.endpointOr404(w, r)
	if ep == nil {
		return
	}
	d := a.hooks.queue(ep, "ping", map[string]any{"message": "This is a mimux webhook test delivery."})
	if d == nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't queue the test delivery.")
		return
	}
	a.sendSoon()
	writeJSONStatus(w, http.StatusAccepted, toDeliveryJSON(*d))
}

// deliveryOr404 loads the {did} delivery and checks it belongs to ep.
func (a *api) deliveryOr404(w http.ResponseWriter, r *http.Request, ep *store.WebhookEndpoint) *store.WebhookDelivery {
	did, err := strconv.ParseInt(chi.URLParam(r, "did"), 10, 64)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "No such delivery.")
		return nil
	}
	d, err := a.store.WebhookDeliveryByID(did)
	if err != nil || d == nil || d.EndpointID != ep.ID {
		apiError(w, http.StatusNotFound, "not_found", "No delivery "+strconv.FormatInt(did, 10)+" on this webhook.")
		return nil
	}
	return d
}

// sendSoon nudges the engine to drain now rather than on its next tick. Its own
// context, because the request's is cancelled the moment we answer. Racing the
// engine's own drain is harmless: delivery is at-least-once and the receiver
// deduplicates on the delivery id.
func (a *api) sendSoon() {
	go a.hooks.drain(context.Background())
}
