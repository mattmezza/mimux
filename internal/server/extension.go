// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// Extension is a route group contributed by a build-tagged add-on — today only
// the ELv2 pro layer in pro/. This file is AGPL and always compiled; what it
// mounts is whatever registered itself, which in the free build is nothing.
// See LICENSING.md.
type Extension struct {
	Pattern string
	Handler http.Handler
}

// Deps is the seam an extension binds to: the client's domain layer without the
// HTTP and template plumbing around it.
//
// Concrete types, deliberately. There is exactly one implementation of each and
// pro/ links into the same binary, so an interface here would be indirection
// with a single implementor on both sides.
// NOTE: struct, not interface — add interfaces when a second implementation
// or a real test double needs one.
type Deps struct {
	Mail  *mail.Manager
	Store *store.Store
	Cfg   *config.Config
}

var extensions []func(Deps) Extension

// RegisterExtension is called from an init() in a build-tagged package. Not
// concurrency-safe and does not need to be: registration happens during package
// initialisation, before main runs.
func RegisterExtension(f func(Deps) Extension) { extensions = append(extensions, f) }

// mountExtensions mounts every registered extension outside the session-cookie
// auth group — extensions are machine-facing and carry their own auth.
func (s *Server) mountExtensions(r chi.Router) {
	deps := Deps{Mail: s.mail, Store: s.store, Cfg: s.cfg}
	for _, f := range extensions {
		e := f(deps)
		r.Mount(e.Pattern, e.Handler)
		slog.Info("extension mounted", "pattern", e.Pattern)
	}
}
