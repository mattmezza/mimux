//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// mintToken creates a token and returns its secret, exactly the way
// Settings → API does. tok.ID is filled in by the insert.
func mintToken(t *testing.T, st *store.Store, tok *store.APIToken) string {
	t.Helper()
	secret := auth.NewAPIToken()
	hash, err := auth.HashPassword(secret)
	if err != nil {
		t.Fatal(err)
	}
	tok.Hash = hash
	if err := st.CreateAPIToken(tok); err != nil {
		t.Fatal(err)
	}
	return secret
}

// probe is the handler behind the middleware in these tests: it echoes the
// token that got through, so a 200 proves the request context was populated.
func probe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"label": tokenOf(r).Label})
}

func call(h http.Handler, secret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/probe", nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// errCode reads the code out of the error envelope, failing if the body isn't
// one — the envelope's shape is part of the API contract.
func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not an error envelope: %s", rec.Body.String())
	}
	if body.Error.Message == "" {
		t.Errorf("error envelope has no message: %s", rec.Body.String())
	}
	return body.Error.Code
}

func TestAuthAcceptsAndRejects(t *testing.T) {
	st := openStore(t)
	a := newTokenAuth(st, 120)
	h := a.require(http.HandlerFunc(probe))

	good := mintToken(t, st, &store.APIToken{Label: "good", Scopes: "mail:read"})
	doomed := &store.APIToken{Label: "doomed"}
	doomedSecret := mintToken(t, st, doomed)
	expired := mintToken(t, st, &store.APIToken{Label: "expired", ExpiresAt: time.Now().Add(-time.Hour)})

	if rec := call(h, good); rec.Code != http.StatusOK {
		t.Fatalf("valid token = %d, body %s", rec.Code, rec.Body.String())
	}
	// Twice: the second request resolves through the digest cache rather than
	// argon2id, and must reach the handler just the same.
	if rec := call(h, good); rec.Code != http.StatusOK {
		t.Fatalf("cached valid token = %d", rec.Code)
	}

	for name, secret := range map[string]string{
		"missing header": "",
		"not a token":    "hunter2",
		"unknown token":  auth.NewAPIToken(),
		"expired token":  expired,
	} {
		rec := call(h, secret)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", name, rec.Code)
			continue
		}
		if code := errCode(t, rec); code != "unauthorized" {
			t.Errorf("%s code = %q, want unauthorized", name, code)
		}
	}

	// Revocation must bite immediately, including for a token already sitting
	// in the digest cache from the successful request just before it.
	if rec := call(h, doomedSecret); rec.Code != http.StatusOK {
		t.Fatalf("token = %d before revoke, want 200", rec.Code)
	}
	if err := st.RevokeAPIToken(doomed.ID); err != nil {
		t.Fatal(err)
	}
	rec := call(h, doomedSecret)
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "unauthorized" {
		t.Errorf("revoked token = %d %s, want 401 unauthorized", rec.Code, rec.Body.String())
	}
}

func TestRequireScope(t *testing.T) {
	st := openStore(t)
	a := newTokenAuth(st, 0)
	h := a.require(requireScope("mail:send")(http.HandlerFunc(probe)))

	readOnly := mintToken(t, st, &store.APIToken{Label: "reader", Scopes: "mail:read"})
	sender := mintToken(t, st, &store.APIToken{Label: "sender", Scopes: "mail:read mail:send"})

	rec := call(h, readOnly)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only token = %d, want 403", rec.Code)
	}
	if code := errCode(t, rec); code != "insufficient_scope" {
		t.Errorf("code = %q, want insufficient_scope", code)
	}
	// The message names the missing scope, so the caller knows which box to tick.
	if body := rec.Body.String(); !strings.Contains(body, "mail:send") {
		t.Errorf("403 body should name the missing scope: %s", body)
	}
	if rec := call(h, sender); rec.Code != http.StatusOK {
		t.Errorf("scoped token = %d, want 200", rec.Code)
	}
}

func TestRateLimit(t *testing.T) {
	st := openStore(t)
	a := newTokenAuth(st, 60) // one per second, so the bucket empties quickly
	h := a.require(http.HandlerFunc(probe))
	secret := mintToken(t, st, &store.APIToken{Label: "busy"})

	var limited *httptest.ResponseRecorder
	for range 62 {
		rec := call(h, secret)
		if rec.Code == http.StatusTooManyRequests {
			limited = rec
			break
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
		}
	}
	if limited == nil {
		t.Fatal("never rate-limited after 62 requests on a budget of 60/min")
	}
	if code := errCode(t, limited); code != "rate_limited" {
		t.Errorf("code = %q, want rate_limited", code)
	}
	if ra := limited.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("Retry-After = %q, want a positive number of seconds", ra)
	}

	// Budgets are per token: a second token is unaffected.
	other := mintToken(t, st, &store.APIToken{Label: "quiet"})
	if rec := call(h, other); rec.Code != http.StatusOK {
		t.Errorf("second token = %d, want its own budget", rec.Code)
	}

	// A limit of 0 turns limiting off.
	off := newTokenAuth(st, 0)
	offH := off.require(http.HandlerFunc(probe))
	for range 200 {
		if rec := call(offH, secret); rec.Code != http.StatusOK {
			t.Fatalf("limit 0 should never limit, got %d", rec.Code)
		}
	}
}

func TestLastUsedUpdate(t *testing.T) {
	st := openStore(t)
	a := newTokenAuth(st, 0)
	h := a.require(http.HandlerFunc(probe))
	tok := &store.APIToken{Label: "used"}
	secret := mintToken(t, st, tok)

	before, _ := st.APITokenByID(tok.ID)
	if !before.LastUsedAt.IsZero() {
		t.Fatalf("unused token has LastUsedAt = %v", before.LastUsedAt)
	}
	if rec := call(h, secret); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	after, _ := st.APITokenByID(tok.ID)
	if after.LastUsedAt.IsZero() {
		t.Fatal("last_used_at not written")
	}

	// Throttled: another request inside the interval must not write again. The
	// backdated stamp would be overwritten if it did.
	if err := st.TouchAPIToken(tok.ID, time.Now().Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rec := call(h, secret); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	again, _ := st.APITokenByID(tok.ID)
	if !again.LastUsedAt.Before(time.Now().Add(-time.Hour)) {
		t.Errorf("last_used_at written twice inside the throttle window: %v", again.LastUsedAt)
	}
}
