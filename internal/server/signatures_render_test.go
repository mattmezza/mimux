package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/store"
)

// TestComposeEmbedsSignature verifies the compose partial embeds the linked
// signature for the From identity (auto flag set) and, for a first-message-only
// signature, that the apply mode rides along so the client can skip it on
// replies.
func TestComposeEmbedsSignature(t *testing.T) {
	s := serverWith(t, nil, func(st *store.Store) {
		if err := st.UpsertAccount(config.Account{Name: "Work", Email: "me@work.com", SenderName: "Me", Provider: "gmail", Password: "p"}); err != nil {
			t.Fatal(err)
		}
		sig := &store.Signature{Name: "Sig", TextPlain: "PLAINSIG", HTML: "<p>H</p>", Markdown: "MD", ApplyMode: "first"}
		if err := st.UpsertSignature(sig); err != nil {
			t.Fatal(err)
		}
		if err := st.SetIdentitySignature("me@work.com", sig.ID); err != nil {
			t.Fatal(err)
		}
	})

	r := chi.NewRouter()
	r.Get("/compose", s.handleComposeNew)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/compose", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/compose status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "PLAINSIG") {
		t.Errorf("compose did not embed the signature variant; body:\n%s", body)
	}
	if !strings.Contains(body, `data-auto="1"`) {
		t.Errorf("fresh compose should set data-auto")
	}
	if !strings.Contains(body, "me@work.com") {
		t.Errorf("compose sig map missing identity address")
	}
	if !strings.Contains(body, "first") {
		t.Errorf("compose sig map missing apply mode")
	}
}

// TestSignaturesManagerRenders exercises the settings signatures partial: the
// CRUD list and the per-identity picker with the linked option selected.
func TestSignaturesManagerRenders(t *testing.T) {
	s := serverWith(t, nil, func(st *store.Store) {
		if err := st.UpsertAccount(config.Account{Name: "Work", Email: "me@work.com", SenderName: "Me", Provider: "gmail", Password: "p"}); err != nil {
			t.Fatal(err)
		}
		sig := &store.Signature{Name: "MySig", TextPlain: "x"}
		_ = st.UpsertSignature(sig)
		_ = st.SetIdentitySignature("me@work.com", sig.ID)
	})
	r := chi.NewRouter()
	r.Get("/settings/signatures", s.handleSignaturesManager)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/signatures", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"MySig", "me@work.com", `hx-post="/settings/signatures/link"`} {
		if !strings.Contains(body, want) {
			t.Errorf("signatures manager missing %q", want)
		}
	}
}
