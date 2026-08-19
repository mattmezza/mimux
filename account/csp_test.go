// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// anyInline matches inline blocks the way a browser sees them — attributes and
// all — so a <script type="module"> or a style attribute added tomorrow fails
// here rather than silently disappearing from the rendered page in production.
var anyInline = regexp.MustCompile(`(?s)<(script|style)([^>]*)>(.*?)</(?:script|style)>`)

// Every inline block a page actually serves must be covered by the hash in the
// header it is served with. The two are computed from the same templates, but
// through different paths: this one goes through html/template, which is free
// to rewrite what it emits inside a <script>.
func TestCSPCoversEveryInlineBlock(t *testing.T) {
	a := &app{}
	if err := a.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	h := a.routes()

	for _, path := range []string{"/", "/success", "/cancel", "/retrieve", "/support"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		policy := rec.Header().Get("Content-Security-Policy")
		if policy == "" {
			t.Fatalf("%s: no Content-Security-Policy header", path)
		}
		body := rec.Body.String()
		if strings.Contains(body, " style=\"") {
			t.Errorf("%s: inline style attribute — no hash can whitelist one", path)
		}
		for _, m := range anyInline.FindAllStringSubmatch(body, -1) {
			if strings.Contains(m[2], "src=") {
				continue // external file, covered by 'self'
			}
			sum := sha256.Sum256([]byte(m[3]))
			want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if !strings.Contains(policy, want) {
				t.Errorf("%s: inline <%s> not whitelisted, expected %s in the policy", path, m[1], want)
			}
		}
	}
}

// The buy button posts to /checkout, which answers 303 to Stripe. form-action
// is enforced on the redirect target too, so dropping the host here blocks the
// purchase — and the browser blames account.mimux.dev for it.
func TestCSPAllowsStripeCheckout(t *testing.T) {
	a := &app{}
	if err := a.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"form-action 'self' https://checkout.stripe.com",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"default-src 'self'",
	} {
		if !strings.Contains(a.csp, want) {
			t.Errorf("policy is missing %q:\n%s", want, a.csp)
		}
	}
}
