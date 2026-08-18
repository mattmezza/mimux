//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	_ "embed"
	"net/http"
)

// openAPISpec is the hand-written OpenAPI 3.1 description of this API, baked
// into the binary so the docs a caller reads are the docs the binary serves.
// docs/ builds the static reference site from this same file.
//
//go:embed openapi.json
var openAPISpec []byte

// handleOpenAPI serves the spec. Unauthenticated on purpose: it is public
// documentation, and a client that cannot yet authenticate is exactly the one
// that needs to read it.
func handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openAPISpec)
}
