// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/store"
)

// webhookLogLines is how many recent attempts the listing shows per endpoint.
// The store keeps 100 and the deliveries screen pages through all of them; this
// is the "what just happened" glance, so it stays short enough to read.
const webhookLogLines = 5

// deliveryPageSize is one page of the deliveries screen.
const deliveryPageSize = 25

// webhookView is one endpoint plus its numbers and its most recent deliveries.
type webhookView struct {
	store.WebhookEndpoint
	Stats      store.WebhookStats
	Deliveries []store.WebhookDelivery
}

// renderWebhooks re-renders the webhook manager. secret is a plaintext signing
// secret that was just generated — shown once, like an API token, even though
// (unlike a token) the database can still read it: pasting it into the
// receiving end is a create-time job, and a secret that stays on screen forever
// is a secret in a screenshot.
func (s *Server) renderWebhooks(w http.ResponseWriter, r *http.Request, secret string) {
	views, err := s.webhookViews()
	if err != nil {
		slog.Error("webhooks: list", "err", err)
		http.Error(w, "Couldn't load webhooks.", http.StatusInternalServerError)
		return
	}
	s.renderPartial(w, "webhooks", map[string]any{
		"CSRF":      auth.EnsureCSRF(w, r, s.secure),
		"Webhooks":  views,
		"Events":    store.WebhookEvents,
		"NewSecret": secret,
	})
}

func (s *Server) webhookViews() ([]webhookView, error) {
	eps, err := s.store.ListWebhookEndpoints()
	if err != nil {
		return nil, err
	}
	views := make([]webhookView, 0, len(eps))
	for _, ep := range eps {
		del, err := s.store.ListWebhookDeliveries(ep.ID, webhookLogLines)
		if err != nil {
			return nil, err
		}
		st, err := s.store.WebhookEndpointStats(ep.ID)
		if err != nil {
			return nil, err
		}
		views = append(views, webhookView{WebhookEndpoint: ep, Stats: st, Deliveries: del})
	}
	return views, nil
}

// handleWebhookCreate registers an endpoint and shows its signing secret once.
func (s *Server) handleWebhookCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ep := &store.WebhookEndpoint{
		URL:    strings.TrimSpace(r.PostFormValue("url")),
		Secret: auth.NewToken(),
		Events: strings.Join(r.PostForm["events"], " "),
		Active: true,
	}
	// A secret the user brought along (their receiver already has one) is used
	// as given; blank means "mint me one", which is the normal path.
	if given := strings.TrimSpace(r.PostFormValue("secret")); given != "" {
		if len(given) < 16 {
			http.Error(w, "A signing secret needs at least 16 characters.", http.StatusBadRequest)
			return
		}
		ep.Secret = given
	}
	if err := s.store.CreateWebhookEndpoint(ep); err != nil {
		// The only failure the user can cause is a bad URL, and the store's
		// message says exactly that.
		http.Error(w, "That endpoint URL isn't a valid http(s) address.", http.StatusBadRequest)
		return
	}
	s.renderWebhooks(w, r, ep.Secret)
}

// handleWebhookPause stops deliveries without losing the queue: the rows stay
// pending and go out when it is switched back on.
func (s *Server) handleWebhookPause(w http.ResponseWriter, r *http.Request) {
	s.setWebhookActive(w, r, false)
}

// handleWebhookEnable turns an endpoint back on, clearing the auto-disabled
// stamp — the way back from "the engine gave up on this one" that doesn't cost
// the user their secret.
func (s *Server) handleWebhookEnable(w http.ResponseWriter, r *http.Request) {
	s.setWebhookActive(w, r, true)
}

func (s *Server) setWebhookActive(w http.ResponseWriter, r *http.Request, active bool) {
	ep := s.webhookOr404(w, r)
	if ep == nil {
		return
	}
	ep.Active = active
	if active {
		ep.AutoDisabledAt = time.Time{}
	}
	if err := s.store.UpdateWebhookEndpoint(ep); err != nil {
		slog.Error("webhooks: set active", "id", ep.ID, "active", active, "err", err)
		http.Error(w, "Couldn't change the webhook.", http.StatusInternalServerError)
		return
	}
	s.renderWebhooks(w, r, "")
}

// handleWebhookSecret rotates the signing key. A blank field mints a fresh
// secret and shows it once; a typed one is taken as given, for a receiver that
// already has a key of its own.
func (s *Server) handleWebhookSecret(w http.ResponseWriter, r *http.Request) {
	ep := s.webhookOr404(w, r)
	if ep == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	secret, generated := strings.TrimSpace(r.PostFormValue("secret")), false
	if secret == "" {
		secret, generated = auth.NewToken(), true
	} else if len(secret) < 16 {
		http.Error(w, "A signing secret needs at least 16 characters.", http.StatusBadRequest)
		return
	}
	if err := s.store.SetWebhookSecret(ep.ID, secret); err != nil {
		slog.Error("webhooks: rotate secret", "id", ep.ID, "err", err) // never the secret
		http.Error(w, "Couldn't change the signing secret.", http.StatusInternalServerError)
		return
	}
	s.mail.Toast("Signing secret updated — the receiver needs the new one")
	shown := ""
	if generated {
		shown = secret
	}
	s.renderWebhooks(w, r, shown)
}

func (s *Server) handleWebhookDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteWebhookEndpoint(id); err != nil {
		slog.Error("webhooks: delete", "id", id, "err", err)
		http.Error(w, "Couldn't delete the webhook.", http.StatusInternalServerError)
		return
	}
	s.renderWebhooks(w, r, "")
}

// handleWebhookDeliveries is the debugging screen for one endpoint: the log,
// filtered and paged, with the exact request body and the receiver's answer.
func (s *Server) handleWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	ep := s.webhookOr404(w, r)
	if ep == nil {
		return
	}
	q := r.URL.Query()
	f := store.WebhookDeliveryFilter{
		Status: q.Get("status"),
		Event:  q.Get("event"),
		Limit:  deliveryPageSize,
	}
	page := atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	f.Offset = (page - 1) * deliveryPageSize
	deliveries, total, err := s.store.FilterWebhookDeliveries(ep.ID, f)
	if err != nil {
		slog.Error("webhooks: deliveries", "id", ep.ID, "err", err)
		http.Error(w, "Couldn't load the delivery log.", http.StatusInternalServerError)
		return
	}
	stats, err := s.store.WebhookEndpointStats(ep.ID)
	if err != nil {
		slog.Error("webhooks: stats", "id", ep.ID, "err", err)
		http.Error(w, "Couldn't load the delivery log.", http.StatusInternalServerError)
		return
	}
	pages := (total + deliveryPageSize - 1) / deliveryPageSize
	data := s.settingsData(w, r)
	// The nav highlights Webhooks: this screen is a room inside that section,
	// not a fifteenth section of its own.
	data["Section"] = "webhooks"
	data["Page"] = *settingsSection("webhooks")
	data["Endpoint"] = ep
	data["Stats"] = stats
	data["Deliveries"] = deliveries
	data["Filter"] = f
	data["Statuses"] = []string{store.WebhookPending, store.WebhookFailed, store.WebhookOK, store.WebhookDead}
	data["Total"] = total
	data["PageNo"] = page
	data["Pages"] = pages
	data["Self"] = deliveriesURL(ep.ID, f, page)
	if page > 1 {
		data["PrevURL"] = deliveriesURL(ep.ID, f, page-1)
	}
	if page < pages {
		data["NextURL"] = deliveriesURL(ep.ID, f, page+1)
	}
	s.renderRequest(w, r, "deliveries", data)
}

// deliveriesURL rebuilds this screen's own URL with one page number changed —
// the pager and the "come back here after a replay" field both need it.
func deliveriesURL(endpointID int64, f store.WebhookDeliveryFilter, page int) string {
	q := url.Values{}
	if f.Status != "" {
		q.Set("status", f.Status)
	}
	if f.Event != "" {
		q.Set("event", f.Event)
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	u := "/settings/webhooks/" + strconv.FormatInt(endpointID, 10) + "/deliveries"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// handleWebhookReplay re-queues one delivery. In the free build the row goes
// back to pending and nothing drains it — the UI says so.
//
// It answers with the webhooks fragment for the listing, or a redirect back to
// the deliveries screen when that is where the button was: same action, two
// callers, and the caller says where it wants to end up.
func (s *Server) handleWebhookReplay(w http.ResponseWriter, r *http.Request) {
	ep := s.webhookOr404(w, r)
	if ep == nil {
		return
	}
	did, err := strconv.ParseInt(chi.URLParam(r, "did"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := s.store.WebhookDeliveryByID(did)
	if err != nil || d == nil || d.EndpointID != ep.ID {
		http.NotFound(w, r)
		return
	}
	if err := s.store.ReplayWebhookDelivery(d.ID, time.Now()); err != nil {
		slog.Error("webhooks: replay", "delivery", d.ID, "err", err)
		http.Error(w, "Couldn't re-queue the delivery.", http.StatusInternalServerError)
		return
	}
	// Only a path on this site, and only under /settings — a redirect target
	// taken from a form is an open redirect unless something says otherwise.
	if back := r.PostFormValue("back"); strings.HasPrefix(back, "/settings/") && !strings.Contains(back, "//") {
		// #nosec G710 -- the /settings/ prefix rules out a scheme or host and the "//" test rules out a protocol-relative URL, so this is a same-site path.
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	s.renderWebhooks(w, r, "")
}

// webhookOr404 loads the endpoint named by the {id} path param.
func (s *Server) webhookOr404(w http.ResponseWriter, r *http.Request) *store.WebhookEndpoint {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	ep, err := s.store.WebhookEndpointByID(id)
	if err != nil || ep == nil {
		http.NotFound(w, r)
		return nil
	}
	return ep
}
