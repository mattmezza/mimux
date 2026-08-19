// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Mutating requests to an extension mount must not be CSRF-checked: they carry
// a bearer token, not a session cookie, and the check rejected every machine
// caller — the REST API, /api/mcp, the CLI — with "invalid CSRF token".
// Everything else must still be checked.
func TestCSRFExceptExtensionMounts(t *testing.T) {
	for _, tc := range []struct {
		path  string
		reach bool
	}{
		{"/api", true},
		{"/api/mcp", true},
		{"/api/v1/messages/send", true},
		{"/apiary", false}, // a sibling path, not a mount: still guarded
		{"/messages/1/read", false},
		// The CLI's code-for-token exchange is the same kind of caller: it
		// carries a one-shot code and reads no cookie. The approval form one
		// path up is an ordinary cookie-authenticated post and stays guarded.
		{cliExchangePath, true},
		{"/cli/auth", false},
	} {
		var reached bool
		h := csrfExcept([]string{"/api", cliExchangePath})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", tc.path, nil))
		if reached != tc.reach {
			t.Errorf("POST %s: reached handler = %v, want %v (status %d)", tc.path, reached, tc.reach, w.Code)
		}
	}
}
