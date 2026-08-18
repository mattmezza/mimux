// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http"
	"strings"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/ext"
)

// extensions builds every add-on registered with internal/ext. Nothing
// registers in the free build, so this is empty there. The types themselves
// live in internal/ext rather than here so that pro/ never imports this
// package — see that package's doc comment and `make verify-boundary`.
//
// Built before the router rather than mounted onto it, because Handler needs
// the mount patterns while it is still assembling the middleware chain (chi
// wants every Use before the first route) — see csrfExcept.
func (s *Server) extensions() []ext.Extension {
	deps := ext.Deps{Mail: s.mail, Store: s.store, Cfg: s.cfg}
	out := make([]ext.Extension, 0, len(ext.All()))
	for _, f := range ext.All() {
		out = append(out, f(deps))
	}
	return out
}

// csrfExcept applies the CSRF check everywhere except under the extension
// mounts. The check defends cookie-authenticated form posts; extensions
// authenticate with bearer tokens and never read the session cookie, so
// applying it there rejected every legitimate machine caller — a POST from
// curl, an MCP tool call, `mimux mail send` — with "invalid CSRF token" and
// defended nothing. Nor can a browser forge those requests: it will not attach
// an Authorization header cross-origin without a preflight this server never
// answers. Free build: no extensions, so no exemptions.
func csrfExcept(patterns []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		guarded := auth.CSRF(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, p := range patterns {
				if r.URL.Path == p || strings.HasPrefix(r.URL.Path, p+"/") {
					next.ServeHTTP(w, r)
					return
				}
			}
			guarded.ServeHTTP(w, r)
		})
	}
}
