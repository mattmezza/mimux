// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/ext"
)

// mountExtensions mounts every add-on registered with internal/ext. Nothing
// registers in the free build, so this is a no-op there. The types themselves
// live in internal/ext rather than here so that pro/ never imports this
// package — see that package's doc comment and `make verify-boundary`.
func (s *Server) mountExtensions(r chi.Router) {
	deps := ext.Deps{Mail: s.mail, Store: s.store, Cfg: s.cfg}
	for _, f := range ext.All() {
		e := f(deps)
		r.Mount(e.Pattern, e.Handler)
		slog.Info("extension mounted", "pattern", e.Pattern)
	}
}
