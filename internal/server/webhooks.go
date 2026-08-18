// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/store"
)

// webhookLogLines is how many recent attempts the Settings log shows per
// endpoint. The store keeps 100; the API serves them all. This is the "what
// just happened" view, so it stays short enough to read.
const webhookLogLines = 10

// webhookView is one endpoint plus its recent deliveries, for the template.
type webhookView struct {
	store.WebhookEndpoint
	Deliveries []store.WebhookDelivery
}

// renderWebhooks re-renders the webhook manager. secret is the plaintext
// signing secret of a just-created endpoint — shown once, like an API token,
// even though (unlike a token) the database can still read it: pasting it into
// the receiving end is a create-time job, and a secret that stays on screen
// forever is a secret in a screenshot.
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
		views = append(views, webhookView{WebhookEndpoint: ep, Deliveries: del})
	}
	return views, nil
}

func (s *Server) handleWebhooksManager(w http.ResponseWriter, r *http.Request) {
	s.renderWebhooks(w, r, "")
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
	if err := s.store.CreateWebhookEndpoint(ep); err != nil {
		// The only failure the user can cause is a bad URL, and the store's
		// message says exactly that.
		http.Error(w, "That endpoint URL isn't a valid http(s) address.", http.StatusBadRequest)
		return
	}
	s.renderWebhooks(w, r, ep.Secret)
}

// handleWebhookEnable turns an endpoint back on, clearing the auto-disabled
// stamp — the way back from "the engine gave up on this one" that doesn't cost
// the user their secret.
func (s *Server) handleWebhookEnable(w http.ResponseWriter, r *http.Request) {
	ep := s.webhookOr404(w, r)
	if ep == nil {
		return
	}
	ep.Active, ep.AutoDisabledAt = true, time.Time{}
	if err := s.store.UpdateWebhookEndpoint(ep); err != nil {
		slog.Error("webhooks: enable", "id", ep.ID, "err", err)
		http.Error(w, "Couldn't enable the webhook.", http.StatusInternalServerError)
		return
	}
	s.renderWebhooks(w, r, "")
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

// handleWebhookReplay re-queues one delivery. In the free build the row goes
// back to pending and nothing drains it — the UI says so.
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
