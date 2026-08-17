//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
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
// Placeholder: the pro layer is announced but not yet implemented. This exists
// so the build-tag split is provable today rather than asserted, and so the
// first real handler has somewhere to go.
func routes(deps ext.Deps) ext.Extension {
	r := chi.NewRouter()
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		// Reads deps so the seam is exercised, not just declared.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"accounts": len(deps.Mail.Status()),
		})
	})
	return ext.Extension{Pattern: "/api", Handler: r}
}
