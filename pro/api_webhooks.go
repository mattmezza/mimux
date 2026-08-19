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

// deliveryDefaultLimit and deliveryMaxLimit bound GET /webhooks/{id}/deliveries.
// The store only ever keeps webhookDeliveryKeep (100) rows per endpoint, so a
// limit past that just returns everything there is.
const deliveryDefaultLimit = 25
const deliveryMaxLimit = 100

// minSecretLen is the same rule the Settings UI enforces on a hand-typed
// signing secret (see internal/server/webhooks.go).
const minSecretLen = 16

type webhookJSON struct {
	ID             int64            `json:"id"`
	URL            string           `json:"url"`
	Events         []string         `json:"events"`
	Active         bool             `json:"active"`
	AutoDisabledAt string           `json:"auto_disabled_at,omitempty"`
	CreatedAt      string           `json:"created_at"`
	Stats          webhookStatsJSON `json:"stats"`
}

func toWebhookJSON(ep store.WebhookEndpoint, st store.WebhookStats) webhookJSON {
	out := webhookJSON{
		ID: ep.ID, URL: ep.URL, Events: ep.EventList(), Active: ep.Active,
		CreatedAt: ep.CreatedAt.UTC().Format(time.RFC3339),
		Stats:     toWebhookStatsJSON(st),
	}
	if out.Events == nil {
		out.Events = []string{}
	}
	if ep.AutoDisabled() {
		out.AutoDisabledAt = ep.AutoDisabledAt.UTC().Format(time.RFC3339)
	}
	return out
}

// webhookStatsJSON is the same numbers the Settings UI shows next to each
// endpoint: whether deliveries are getting through, how many are stuck, and
// when it last tried. Folded from the last webhookDeliveryKeep deliveries,
// not all time — see store.WebhookStats.
type webhookStatsJSON struct {
	Total          int    `json:"total"`
	SuccessRate    *int   `json:"success_rate,omitempty"` // absent until something has settled
	Failing        int    `json:"failing"`                // attempted at least once and not yet delivered
	Pending        int    `json:"pending"`
	LastDeliveryAt string `json:"last_delivery_at,omitempty"`
	LastStatus     string `json:"last_status,omitempty"`
	LastStatusCode int    `json:"last_status_code,omitempty"`
}

func toWebhookStatsJSON(st store.WebhookStats) webhookStatsJSON {
	out := webhookStatsJSON{
		Total: st.Total, Failing: st.Failing(), Pending: st.Pending,
		LastStatus: st.LastStatus, LastStatusCode: st.LastCode,
	}
	if st.Settled() > 0 {
		rate := st.SuccessRate()
		out.SuccessRate = &rate
	}
	if !st.LastAt.IsZero() {
		out.LastDeliveryAt = st.LastAt.UTC().Format(time.RFC3339)
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
	ResponseBody   string `json:"response_body,omitempty"`
	DurationMS     int    `json:"duration_ms,omitempty"`
	NextAttemptAt  string `json:"next_attempt_at,omitempty"`
	CreatedAt      string `json:"created_at"`
	DeliveredAt    string `json:"delivered_at,omitempty"`
}

func toDeliveryJSON(d store.WebhookDelivery) deliveryJSON {
	out := deliveryJSON{
		ID: d.ID, DeliveryID: d.DeliveryID, Event: d.EventType, Status: d.Status,
		Attempts: d.Attempts, LastStatusCode: d.LastStatusCode, LastError: d.LastError,
		ResponseBody: d.ResponseBody, DurationMS: d.DurationMS,
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

// webhookJSONFor folds ep together with its stats, the same pair the Settings
// UI renders as one webhookView row.
func (a *api) webhookJSONFor(ep store.WebhookEndpoint) (webhookJSON, error) {
	st, err := a.store.WebhookEndpointStats(ep.ID)
	if err != nil {
		return webhookJSON{}, err
	}
	return toWebhookJSON(ep, st), nil
}

func (a *api) handleListWebhooks(w http.ResponseWriter, _ *http.Request) {
	eps, err := a.store.ListWebhookEndpoints()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't list webhooks.")
		return
	}
	out := make([]webhookJSON, 0, len(eps))
	for _, ep := range eps {
		wj, err := a.webhookJSONFor(ep)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "internal", "Couldn't list webhooks.")
			return
		}
		out = append(out, wj)
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
	wj, err := a.webhookJSONFor(*ep)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Webhook created, but couldn't read it back.")
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"webhook": wj,
		"secret":  ep.Secret,
		"note":    "This secret is shown once. Store it now — it is not returned by any other endpoint. To change it, POST /webhooks/{id}/secret.",
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
	wj, err := a.webhookJSONFor(*ep)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't read the webhook back.")
		return
	}
	writeJSON(w, wj)
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

// handlePauseWebhook and handleResumeWebhook toggle active without touching
// url or events — the same field the PATCH handler sets, exposed as its own
// route because "pause this" is the common case and shouldn't require
// resending url/events to avoid clobbering them.
func (a *api) handlePauseWebhook(w http.ResponseWriter, r *http.Request) {
	a.setWebhookActive(w, r, false)
}

// handleResumeWebhook turns an endpoint back on, clearing any auto-disabled
// stamp — the way back from "the engine gave up on this one".
func (a *api) handleResumeWebhook(w http.ResponseWriter, r *http.Request) {
	a.setWebhookActive(w, r, true)
}

func (a *api) setWebhookActive(w http.ResponseWriter, r *http.Request, active bool) {
	ep := a.endpointOr404(w, r)
	if ep == nil {
		return
	}
	ep.Active = active
	if active {
		ep.AutoDisabledAt = time.Time{}
	}
	if err := a.store.UpdateWebhookEndpoint(ep); err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't change the webhook.")
		return
	}
	wj, err := a.webhookJSONFor(*ep)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't read the webhook back.")
		return
	}
	writeJSON(w, wj)
}

// handleSetWebhookSecret rotates the signing key, or sets one for the first
// time in the unlikely case a caller wants a fixed secret from the start. Same
// rule as the Settings UI: a secret you supply must be at least minSecretLen
// characters, or leave it blank and one is generated. Either way it is in this
// response and nowhere else — SetWebhookSecret only stores it, it never reads
// it back.
func (a *api) handleSetWebhookSecret(w http.ResponseWriter, r *http.Request) {
	ep := a.endpointOr404(w, r)
	if ep == nil {
		return
	}
	var req struct {
		Secret string `json:"secret"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	secret := req.Secret
	if secret == "" {
		secret = auth.NewToken()
	} else if len(secret) < minSecretLen {
		apiError(w, http.StatusBadRequest, "invalid_request",
			"A signing secret needs at least "+strconv.Itoa(minSecretLen)+" characters.")
		return
	}
	if err := a.store.SetWebhookSecret(ep.ID, secret); err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't change the signing secret.")
		return
	}
	ep.Secret = secret
	wj, err := a.webhookJSONFor(*ep)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Secret changed, but couldn't read the webhook back.")
		return
	}
	writeJSON(w, map[string]any{
		"webhook": wj,
		"secret":  secret,
		"note":    "This secret is shown once. Store it now — it is not returned by any other endpoint.",
	})
}

// handleWebhookDeliveries reads the log, filtered by ?status and ?event (the
// same two the deliveries screen filters on) and paged with ?limit and
// ?cursor. cursor is the offset to resume from — the value handed back as
// next_cursor, opaque as far as a caller needs to know.
func (a *api) handleWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	ep := a.endpointOr404(w, r)
	if ep == nil {
		return
	}
	q := r.URL.Query()
	limit := deliveryDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > deliveryMaxLimit {
			apiError(w, http.StatusBadRequest, "invalid_request", "limit must be 1-"+strconv.Itoa(deliveryMaxLimit)+".")
			return
		}
		limit = n
	}
	offset := 0
	if v := q.Get("cursor"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			apiError(w, http.StatusBadRequest, "invalid_request", "cursor is not valid.")
			return
		}
		offset = n
	}
	f := store.WebhookDeliveryFilter{Status: q.Get("status"), Event: q.Get("event"), Limit: limit, Offset: offset}
	dels, total, err := a.store.FilterWebhookDeliveries(ep.ID, f)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't read the delivery log.")
		return
	}
	out := make([]deliveryJSON, 0, len(dels))
	for _, d := range dels {
		out = append(out, toDeliveryJSON(d))
	}
	next := ""
	if offset+len(dels) < total {
		next = strconv.Itoa(offset + len(dels))
	}
	writeList(w, out, next)
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
	data := map[string]any{"message": "This is a mimux webhook test delivery."}
	// The ping does not go through fire (it targets one endpoint, whatever it is
	// subscribed to), so the live listeners are told here — a test delivery is
	// the first thing anyone debugging with `webhooks listen` reaches for.
	a.hooks.tee("ping", data)
	d := a.hooks.queue(ep, "ping", data)
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
