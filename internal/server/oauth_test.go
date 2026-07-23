package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/mail"
	"github.com/mattmezza/sm/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertAccount(config.Account{Name: "Personal", Provider: "gmail", Email: "me@gmail.com", Auth: "oauth2", OAuth2ClientID: "cid", OAuth2ClientSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Server: config.Server{BaseURL: "https://mail.example.com"}}
	srv, err := New(cfg, st, mail.NewManager(cfg, st), "test")
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// oauthRouter mounts just the OAuth routes (no auth middleware) for testing.
func oauthRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/oauth/{name}/start", s.handleOAuthStart)
	r.Get("/oauth/callback", s.handleOAuthCallback)
	return r
}

func TestOAuthStartRedirectsToProvider(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	oauthRouter(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/Personal/start", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Host != "accounts.google.com" {
		t.Errorf("redirect host = %q, want accounts.google.com", loc.Host)
	}
	// State cookie set, and its nonce matches the state query param.
	var nonce string
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthStateCookie {
			nonce = c.Value
		}
	}
	if nonce == "" {
		t.Fatal("no state cookie set")
	}
	_, stateNonce, _ := strings.Cut(loc.Query().Get("state"), ":")
	if stateNonce != nonce {
		t.Errorf("state nonce %q != cookie nonce %q", stateNonce, nonce)
	}
	if loc.Query().Get("access_type") != "offline" {
		t.Errorf("access_type = %q, want offline", loc.Query().Get("access_type"))
	}
}

func TestOAuthStartUnknownAccount(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	oauthRouter(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/Nope/start", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestOAuthCallbackRejectsBadState(t *testing.T) {
	s := testServer(t)
	// Cookie nonce and state nonce disagree → reject.
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback?state=Personal:aaa&code=x", nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: "bbb"})
	rec := httptest.NewRecorder()
	oauthRouter(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
