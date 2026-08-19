// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/store"
)

func licenceRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/settings/licence", s.renderLicence)
	r.Post("/settings/licence", s.handleLicenceSave)
	r.Post("/settings/licence/remove", s.handleLicenceRemove)
	return r
}

const testLicenceKey = "mimuxlic1.eyJpZCI6ImxpY190ZXN0IiwiZW1haWwiOiJhbGljZUBleGFtcGxlLmNvbSJ9.c2lnbmF0dXJl"

// TestLicenceSaveRemove: the key round-trips into app_settings, is never
// rendered in full, and Remove clears both it and the stale status line.
func TestLicenceSaveRemove(t *testing.T) {
	s := serverWith(t, nil, nil)
	r := licenceRouter(s)

	rec := postAPIToken(t, r, "/settings/licence", url.Values{"licence_key": {"  " + testLicenceKey + "  "}})
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
	}
	got, ok := s.store.Setting(store.SettingLicenceKey)
	if !ok || got != testLicenceKey {
		t.Fatalf("stored key = %q, %v", got, ok)
	}
	if strings.Contains(rec.Body.String(), testLicenceKey) {
		t.Error("the licence block renders the whole key back")
	}
	if !strings.Contains(rec.Body.String(), testLicenceKey[:18]) {
		t.Errorf("the licence block does not show a recognisable prefix: %s", rec.Body.String())
	}

	// A saved key drops the previous verdict rather than presenting it as this
	// key's; the pro layer writes a new one within the minute.
	if err := s.store.SetSetting(store.SettingLicenceStatus, "Annual licence, expires 2027-01-01."); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/licence", nil))
	if !strings.Contains(rec.Body.String(), "Annual licence, expires 2027-01-01.") {
		t.Error("the stored status line is not rendered")
	}
	if rec := postAPIToken(t, r, "/settings/licence", url.Values{"licence_key": {testLicenceKey}}); rec.Code != http.StatusOK {
		t.Fatalf("re-save = %d", rec.Code)
	}
	if v, _ := s.store.Setting(store.SettingLicenceStatus); v != "" {
		t.Errorf("status survived a key change: %q", v)
	}

	rec = postAPIToken(t, r, "/settings/licence/remove", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove = %d: %s", rec.Code, rec.Body.String())
	}
	if v, _ := s.store.Setting(store.SettingLicenceKey); v != "" {
		t.Errorf("key survived remove: %q", v)
	}
	if !strings.Contains(rec.Body.String(), "No licence key") {
		t.Error("the empty state is not rendered after remove")
	}
}

func TestLicenceSaveRejectsBlank(t *testing.T) {
	s := serverWith(t, nil, nil)
	r := licenceRouter(s)
	if rec := postAPIToken(t, r, "/settings/licence", url.Values{"licence_key": {"   "}}); rec.Code != http.StatusBadRequest {
		t.Errorf("blank key = %d, want 400", rec.Code)
	}
	if v, _ := s.store.Setting(store.SettingLicenceKey); v != "" {
		t.Errorf("blank key was stored: %q", v)
	}
}

// TestLicenceEnvWins: with MIMUX_LICENCE_KEY set, the block says so — otherwise
// a user would edit a key that nothing reads.
func TestLicenceEnvWins(t *testing.T) {
	s := serverWith(t, nil, nil)
	s.cfg.LicenceKey = testLicenceKey
	rec := httptest.NewRecorder()
	licenceRouter(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/licence", nil))
	if !strings.Contains(rec.Body.String(), "MIMUX_LICENCE_KEY") {
		t.Errorf("env precedence is not surfaced: %s", rec.Body.String())
	}
}

// TestSettingsPageHasLicenceBlock: the block is on the settings page itself,
// with the honest note that this build may not be the one enforcing it.
func TestSettingsPageHasLicenceBlock(t *testing.T) {
	s := serverWith(t, nil, nil)
	body := renderSection(t, s, "licence")
	for _, want := range []string{
		`id="licence"`, `name="licence_key"`, `hx-post="/settings/licence"`,
		"is part of mimux pro", "No status yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}
