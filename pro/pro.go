//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/ext"
)

// Registration happens at package-init time. cmd/mimux imports this package with
// a blank import guarded by the same `pro` build tag, so in the free build this
// init never exists.
func init() {
	ext.Register(routes)
}

// routes is the whole pro surface's mount point. deps carries the client's
// domain layer — see ext.Deps.
//
// Note what is NOT imported above: internal/server. That is enforced by
// `make verify-boundary`. Anything this package needs that currently lives as a
// private method on *server.Server has to move down into internal/mail or
// internal/store first, so both the HTML handler and this one share it.
//
// The extension mounts outside the session-cookie auth group (see
// server.Handler), and nothing here ever reads that cookie: API callers
// authenticate with a personal access token minted in Settings → API.
func routes(deps ext.Deps) ext.Extension {
	r := chi.NewRouter()
	// Unauthenticated on purpose: a health probe that needs a credential is a
	// health probe nobody wires up. It exposes a count, not any mail.
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "accounts": len(deps.Mail.Status())})
	})

	tokens := newTokenAuth(deps.Store, deps.Cfg.API.RateLimitPerMinute)
	// The webhook delivery engine: subscribes to the mail hub and drains the
	// delivery queue for as long as the process lives. Started here because
	// this is the pro layer's only entry point — the free build has the
	// endpoints table and the Settings UI, and nothing that posts.
	hooks := newWebhooks(deps)
	go hooks.run(context.Background())
	a := newAPI(deps, hooks)
	r.Route("/v1", func(r chi.Router) {
		// Outside the auth group on purpose: the spec is public documentation,
		// and the client that most needs to read it is the one that hasn't got
		// a working token yet.
		r.Get("/openapi.json", handleOpenAPI)
		r.Group(func(r chi.Router) {
			r.Use(tokens.require)
			r.Get("/tokens/self", handleTokenSelf)
			a.mount(r)
		})
	})
	return ext.Extension{Pattern: "/api", Handler: r}
}

// handleTokenSelf describes the calling token. Scope-free by design: it tells a
// client what its own credential may do, which is exactly what a client with a
// too-narrow token needs in order to find that out.
func handleTokenSelf(w http.ResponseWriter, r *http.Request) {
	tok := tokenOf(r)
	writeJSON(w, map[string]any{"label": tok.Label, "scopes": tok.ScopeList()})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
