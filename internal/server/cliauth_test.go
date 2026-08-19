// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func cliAuthRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/cli/auth", s.handleCLIAuthForm)
	r.Post("/cli/auth", s.handleCLIAuthApprove)
	r.Post(cliExchangePath, s.handleCLIExchange)
	return r
}

const testVerifier = "a-verifier-only-the-cli-holds"

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// startCLIAuth walks the GET, so the test carries the same nonce cookie a
// browser would, then posts the approval form.
func startCLIAuth(t *testing.T, s *Server, r http.Handler, query string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	get := httptest.NewRecorder()
	r.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/cli/auth?"+query, nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET /cli/auth = %d: %s", get.Code, get.Body.String())
	}
	var nonce string
	for _, c := range get.Result().Cookies() {
		if c.Name == cliAuthNonce {
			nonce = c.Value
		}
	}
	if nonce == "" {
		t.Fatal("GET /cli/auth set no nonce cookie")
	}
	form.Set("nonce", nonce)
	req := httptest.NewRequest(http.MethodPost, "/cli/auth", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cliAuthNonce, Value: nonce})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// codeFromLocation pulls the one-shot code out of the loopback redirect, which
// is the only place it is ever published.
func codeFromLocation(t *testing.T, loc string) string {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location %q: %v", loc, err)
	}
	return u.Query().Get("code")
}

func exchange(t *testing.T, r http.Handler, code, verifier string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"code": code, "verifier": verifier})
	req := httptest.NewRequest(http.MethodPost, cliExchangePath, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// The happy path: the form's ticked scopes are the grant, the redirect points
// at the CLI's loopback listener, and the code buys the token exactly once.
func TestCLIAuthApproveAndExchange(t *testing.T) {
	s := serverWith(t, nil, nil)
	s.pro = true
	r := cliAuthRouter(s)

	ch := challengeFor(testVerifier)
	query := url.Values{
		"port": {"49711"}, "state": {"st4te"}, "challenge": {ch},
		"name": {"cli @ laptop"}, "scopes": {"mail:read mail:send"},
	}.Encode()
	// The query asked for send as well; the human unticked it in the browser, so
	// the form is what counts.
	rec := startCLIAuth(t, s, r, query, url.Values{
		"port": {"49711"}, "state": {"st4te"}, "challenge": {ch},
		"label": {"laptop"}, "scopes": {"mail:read", "accounts:read", "admin:everything"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "http://127.0.0.1:49711/callback?") {
		t.Fatalf("Location = %q", loc)
	}
	if u, _ := url.Parse(loc); u.Query().Get("state") != "st4te" {
		t.Errorf("state not echoed back: %q", loc)
	}

	toks, _ := s.store.ListAPITokens()
	if len(toks) != 1 || toks[0].Label != "laptop" {
		t.Fatalf("tokens = %+v", toks)
	}
	if toks[0].Scopes != "mail:read accounts:read" {
		t.Errorf("scopes = %q — the form's checkboxes are the grant", toks[0].Scopes)
	}

	code := codeFromLocation(t, loc)
	ex := exchange(t, r, code, testVerifier)
	if ex.Code != http.StatusOK {
		t.Fatalf("exchange = %d: %s", ex.Code, ex.Body.String())
	}
	var got struct {
		Token, Label string
		Scopes       []string
	}
	if err := json.Unmarshal(ex.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Token, "mimux_pat_") || got.Label != "laptop" {
		t.Errorf("exchange body = %+v", got)
	}
	if strings.Join(got.Scopes, " ") != "mail:read accounts:read" {
		t.Errorf("scopes = %v", got.Scopes)
	}

	// Replay: the code was deleted on read.
	if again := exchange(t, r, code, testVerifier); again.Code != http.StatusNotFound {
		t.Errorf("replay = %d, want 404", again.Code)
	}
}

// The port ends up in a Location header, so anything that isn't a plausible
// loopback listener is refused before a token is minted.
func TestCLIAuthRejectsBadPort(t *testing.T) {
	s := serverWith(t, nil, nil)
	s.pro = true
	r := cliAuthRouter(s)
	ch := challengeFor(testVerifier)
	for _, port := range []string{"0", "99999", "abc", "80", "", "-1", "49711 "} {
		q := url.Values{"port": {port}, "state": {"s"}, "challenge": {ch}}.Encode()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cli/auth?"+q, nil))
		if port == "49711 " { // trimmed, so this one is fine
			if rec.Code != http.StatusOK {
				t.Errorf("port %q = %d, want 200", port, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("port %q = %d, want 400", port, rec.Code)
		}
	}
	if toks, _ := s.store.ListAPITokens(); len(toks) != 0 {
		t.Errorf("a rejected port still minted %d tokens", len(toks))
	}
}

func TestCLIAuthExchangeRejects(t *testing.T) {
	s := serverWith(t, nil, nil)
	s.pro = true
	r := cliAuthRouter(s)
	ch := challengeFor(testVerifier)
	newCode := func(t *testing.T) string {
		t.Helper()
		q := url.Values{"port": {"49712"}, "state": {"s"}, "challenge": {ch}}.Encode()
		rec := startCLIAuth(t, s, r, q, url.Values{
			"port": {"49712"}, "state": {"s"}, "challenge": {ch}, "label": {"x"}, "scopes": {"mail:read"},
		})
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("approve = %d: %s", rec.Code, rec.Body.String())
		}
		return codeFromLocation(t, rec.Header().Get("Location"))
	}

	if rec := exchange(t, r, newCode(t), "not-the-verifier"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong verifier = %d, want 401", rec.Code)
	}
	if rec := exchange(t, r, "never-issued", testVerifier); rec.Code != http.StatusNotFound {
		t.Errorf("unknown code = %d, want 404", rec.Code)
	}

	// Expired: age the ticket rather than waiting two minutes for it.
	code := newCode(t)
	s.cliMu.Lock()
	tk := s.cliTickets[code]
	tk.expiresAt = time.Now().Add(-time.Second)
	s.cliTickets[code] = tk
	s.cliMu.Unlock()
	if rec := exchange(t, r, code, testVerifier); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired code = %d, want 401", rec.Code)
	}
}

// A stale tab — one whose nonce cookie is gone or never matched — must not be
// able to mint a token by being clicked.
func TestCLIAuthNeedsFreshNonce(t *testing.T) {
	s := serverWith(t, nil, nil)
	s.pro = true
	r := cliAuthRouter(s)
	ch := challengeFor(testVerifier)
	form := url.Values{
		"port": {"49713"}, "state": {"s"}, "challenge": {ch},
		"label": {"x"}, "scopes": {"mail:read"}, "nonce": {"a-nonce-from-an-hour-ago"},
	}
	req := httptest.NewRequest(http.MethodPost, "/cli/auth", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stale approve = %d, want 400", rec.Code)
	}
	if toks, _ := s.store.ListAPITokens(); len(toks) != 0 {
		t.Errorf("a stale tab minted %d tokens", len(toks))
	}
}

// A free build shows the conversion notice and refuses to mint, the same as
// Settings → API.
func TestCLIAuthRefusedInFreeBuild(t *testing.T) {
	s := serverWith(t, nil, nil) // s.pro is false
	r := cliAuthRouter(s)
	ch := challengeFor(testVerifier)
	q := url.Values{"port": {"49714"}, "state": {"s"}, "challenge": {ch}}.Encode()

	get := httptest.NewRecorder()
	r.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/cli/auth?"+q, nil))
	body := get.Body.String()
	if strings.Contains(body, ">Approve<") {
		t.Error("a free build still offers the Approve button")
	}
	if !strings.Contains(body, "mimux.dev/pricing") {
		t.Errorf("free build page missing the pro notice:\n%s", body)
	}

	form := url.Values{"port": {"49714"}, "state": {"s"}, "challenge": {ch}, "label": {"x"}}
	req := httptest.NewRequest(http.MethodPost, "/cli/auth", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("free build approve = %d, want 403", rec.Code)
	}
	if toks, _ := s.store.ListAPITokens(); len(toks) != 0 {
		t.Errorf("a free build minted %d tokens", len(toks))
	}
}

// --no-browser has nowhere to redirect, so the code is put on the page instead.
func TestCLIAuthNoBrowserShowsCode(t *testing.T) {
	s := serverWith(t, nil, nil)
	s.pro = true
	r := cliAuthRouter(s)
	ch := challengeFor(testVerifier)
	q := url.Values{"port": {"49715"}, "state": {"s"}, "challenge": {ch}, "nb": {"1"}}.Encode()
	rec := startCLIAuth(t, s, r, q, url.Values{
		"port": {"49715"}, "state": {"s"}, "challenge": {ch},
		"label": {"headless"}, "scopes": {"mail:read"}, "nb": {"1"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d (want a page, not a redirect): %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Error("--no-browser still redirected to a loopback listener")
	}
	var code string
	s.cliMu.Lock()
	for c := range s.cliTickets {
		code = c
	}
	s.cliMu.Unlock()
	if code == "" || !strings.Contains(rec.Body.String(), code) {
		t.Errorf("the page does not show the code to paste back")
	}
}

// The post-login hop is a same-site path or nothing: a login form that
// redirects wherever its query string says is an open redirect.
func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/cli/auth?port=1&state=s": "/cli/auth?port=1&state=s",
		"/settings":                "/settings",
		"":                         "/",
		"https://evil.example":     "/",
		"//evil.example":           "/",
		"javascript:alert(1)":      "/",
		"http://127.0.0.1:1/x":     "/",
		"settings":                 "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
