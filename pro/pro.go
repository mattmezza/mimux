//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/server"
)

// Registration happens at package-init time. cmd/mimux imports this package with
// a blank import guarded by the same `pro` build tag, so in the free build this
// init never exists.
func init() {
	server.RegisterExtension(routes)
}

// routes is the whole pro surface's mount point. deps carries the client's
// domain layer (mail manager, store, config) — see server.Deps.
//
// Placeholder: the pro layer is announced but not yet implemented. This exists
// so the build-tag split is provable today rather than asserted, and so the
// first real handler has somewhere to go.
func routes(deps server.Deps) server.Extension {
	r := chi.NewRouter()
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		// Reads deps so the seam is exercised, not just declared.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"accounts": len(deps.Mail.Status()),
		})
	})
	return server.Extension{Pattern: "/api", Handler: r}
}
