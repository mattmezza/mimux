package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func settingsRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/settings", s.handleSettings)
	r.Post("/settings", s.handleSettingsSave)
	return r
}

func postSettings(t *testing.T, r http.Handler, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestAvatarDependentsSurviveAvatarsOff pins the fix for the disabled-input
// trap: show_favicon, hide_avatar_mobile and avatar_shape are disabled in the
// UI (and so never submitted by the browser) whenever show_avatar is off.
// handleSettingsSave must preserve their stored values in that case rather
// than reading the absent form fields as "off"/"circle" and overwriting them
// — otherwise turning avatars off and back on would silently reset a user's
// favicon/shape/mobile choice.
func TestAvatarDependentsSurviveAvatarsOff(t *testing.T) {
	s := serverWith(t, nil, nil)
	r := settingsRouter(s)

	// 1. Avatars on, favicon on, shape square — the baseline a user picked.
	on := url.Values{
		"show_avatar":  {"1"},
		"show_favicon": {"1"},
		"avatar_shape": {"square"},
	}
	if rec := postSettings(t, r, on); rec.Code != http.StatusSeeOther {
		t.Fatalf("save (avatars on) status = %d, body: %s", rec.Code, rec.Body.String())
	}
	p := s.store.GetPrefs()
	if !p.ShowFavicon || p.AvatarShape != "square" {
		t.Fatalf("baseline not saved: %+v", p)
	}

	// 2. Save again with avatars off. The browser wouldn't submit
	// show_favicon/avatar_shape at all (disabled inputs), so this form omits
	// them exactly like a real disabled submit would.
	off := url.Values{} // show_avatar, show_favicon, avatar_shape all absent
	if rec := postSettings(t, r, off); rec.Code != http.StatusSeeOther {
		t.Fatalf("save (avatars off) status = %d, body: %s", rec.Code, rec.Body.String())
	}
	p = s.store.GetPrefs()
	if p.ShowAvatar {
		t.Fatalf("show_avatar should be off: %+v", p)
	}
	if !p.ShowFavicon || p.AvatarShape != "square" {
		t.Fatalf("dependents must survive avatars-off save, got %+v", p)
	}

	// 3. Re-enable avatars. A real browser re-submits show_favicon/avatar_shape
	// here too: those inputs were only *disabled* (not reset), and the page
	// that rendered them read the just-preserved values back from GetPrefs, so
	// their checked/selected state — and thus what gets posted once re-enabled
	// — is still favicon=on, shape=square.
	backOn := url.Values{"show_avatar": {"1"}, "show_favicon": {"1"}, "avatar_shape": {"square"}}
	if rec := postSettings(t, r, backOn); rec.Code != http.StatusSeeOther {
		t.Fatalf("save (avatars back on) status = %d, body: %s", rec.Code, rec.Body.String())
	}
	p = s.store.GetPrefs()
	if !p.ShowAvatar || !p.ShowFavicon || p.AvatarShape != "square" {
		t.Fatalf("favicon/shape did not survive the round trip: %+v", p)
	}
}

// TestAvatarShapeValidated checks an out-of-allowlist shape falls back to the
// default instead of being stored verbatim.
func TestAvatarShapeValidated(t *testing.T) {
	s := serverWith(t, nil, nil)
	r := settingsRouter(s)
	form := url.Values{"show_avatar": {"1"}, "avatar_shape": {"</style><script>evil"}}
	if rec := postSettings(t, r, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if got := s.store.GetPrefs().AvatarShape; got != "circle" {
		t.Errorf("AvatarShape = %q, want the circle fallback", got)
	}
}

// TestSettingsPageGroupsAvatarControls checks the avatar controls render
// together (master toggle + nested favicon/mobile/shape) with the Alpine
// disable wiring, so the shape picker and its siblings actually exist and are
// reachable through the master toggle.
func TestSettingsPageGroupsAvatarControls(t *testing.T) {
	s := serverWith(t, nil, nil)
	rec := httptest.NewRecorder()
	s.handleSettings(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`name="show_avatar"`, `name="show_favicon"`, `name="hide_avatar_mobile"`,
		`name="avatar_shape"`, `:disabled="!avatarsOn"`, `value="circle"`, `value="rounded"`, `value="square"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}
