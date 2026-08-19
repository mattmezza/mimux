// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/auth"
)

func apiTokenRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/settings/api", s.handleAPITokensManager)
	r.Post("/settings/api", s.handleAPITokenCreate)
	r.Post("/settings/api/{id}/revoke", s.handleAPITokenRevoke)
	return r
}

func postAPIToken(t *testing.T, r http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestAPITokenCreateShowsSecretOnce pins the one-shot reveal: the plaintext is
// rendered by the create response and by nothing afterwards, because only its
// hash was stored.
func TestAPITokenCreateShowsSecretOnce(t *testing.T) {
	s := serverWith(t, nil, nil)
	r := apiTokenRouter(s)

	rec := postAPIToken(t, r, "/settings/api", url.Values{
		"label":  {"Home Assistant"},
		"scopes": {"mail:read", "mail:send", "admin:everything"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "shown only once") {
		t.Error("create response does not warn that the token is shown once")
	}
	i := strings.Index(body, auth.APITokenPrefix)
	if i < 0 {
		t.Fatalf("create response does not contain the token: %s", body)
	}
	secret := body[i:]
	secret = secret[:strings.IndexAny(secret, "<\" ")]

	toks, err := s.store.ListAPITokens()
	if err != nil || len(toks) != 1 {
		t.Fatalf("ListAPITokens = %d tokens, %v", len(toks), err)
	}
	if toks[0].Label != "Home Assistant" {
		t.Errorf("label = %q", toks[0].Label)
	}
	if toks[0].Scopes != "mail:read mail:send" {
		t.Errorf("scopes = %q, want the unknown one dropped", toks[0].Scopes)
	}
	if strings.Contains(toks[0].Hash, secret) {
		t.Error("the stored hash contains the secret")
	}
	if !auth.VerifyPassword(secret, toks[0].Hash) {
		t.Error("the rendered secret does not verify against the stored hash")
	}

	// Re-rendering the manager must not show it again.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/api", nil))
	if strings.Contains(rec.Body.String(), secret) {
		t.Error("the token is shown again on a plain manager render")
	}

	// Revoke: the row survives, marked, and loses its Revoke button.
	rec = postAPIToken(t, r, "/settings/api/"+strconv.FormatInt(toks[0].ID, 10)+"/revoke", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "revoked") {
		t.Error("revoked token is not marked as such")
	}
	got, _ := s.store.APITokenByID(toks[0].ID)
	if got == nil || got.Live() {
		t.Errorf("token still live after revoke: %+v", got)
	}
}

func TestAPITokenCreateRejectsBadInput(t *testing.T) {
	s := serverWith(t, nil, nil)
	r := apiTokenRouter(s)

	if rec := postAPIToken(t, r, "/settings/api", url.Values{"label": {"  "}}); rec.Code != http.StatusBadRequest {
		t.Errorf("blank label = %d, want 400", rec.Code)
	}
	if rec := postAPIToken(t, r, "/settings/api", url.Values{"label": {"x"}, "expires": {"tomorrow"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("junk expiry = %d, want 400", rec.Code)
	}
	if toks, _ := s.store.ListAPITokens(); len(toks) != 0 {
		t.Errorf("rejected requests still created %d tokens", len(toks))
	}

	// No scope ticked still yields a usable read-only token rather than one
	// that authenticates and can do nothing.
	if rec := postAPIToken(t, r, "/settings/api", url.Values{"label": {"bare"}}); rec.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	toks, _ := s.store.ListAPITokens()
	if len(toks) != 1 || toks[0].Scopes != "mail:read" {
		t.Errorf("default scopes = %+v", toks)
	}
	if !toks[0].ExpiresAt.IsZero() {
		t.Errorf("blank expiry should mean never, got %v", toks[0].ExpiresAt)
	}
}

// TestSettingsPageHasAPITab checks the API tab is wired into the settings page
// itself (nav entry + the manager partial with its data), not only reachable at
// its own endpoint.
func TestSettingsPageHasAPITab(t *testing.T) {
	s := serverWith(t, nil, nil)
	body := renderSection(t, s, "api")
	for _, want := range []string{
		`href="/settings/api"`, `id="api-tokens"`, `name="label"`,
		`name="scopes" value="mail:read"`, `name="expires"`, `hx-post="/settings/api"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}

// TestAPITokenExpiryIsEndOfDay: a date picked in the browser means "valid
// through that day", not "dead at midnight of that morning".
func TestAPITokenExpiryIsEndOfDay(t *testing.T) {
	got, err := parseTokenExpiry("2030-01-31")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2030, 1, 31, 23, 59, 59, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseTokenExpiry = %v, want %v", got, want)
	}
	if blank, err := parseTokenExpiry(""); err != nil || !blank.IsZero() {
		t.Errorf("blank expiry = %v, %v", blank, err)
	}
}
