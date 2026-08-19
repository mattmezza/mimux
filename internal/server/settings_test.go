// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

func settingsRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/settings", s.handleSettings)
	r.Post("/settings", s.handleSettingsSave)
	r.Get("/settings/{section}", s.handleSettingsSection)
	return r
}

// renderSection renders one settings section page the way the router does.
// Settings is a page per section now, so a test that wants the markup has to
// say which screen it means.
func renderSection(t *testing.T, s *Server, section string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	settingsRouter(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/"+section, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings/%s = %d", section, rec.Code)
	}
	return rec.Body.String()
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

// TestSectionSaveLeavesOtherSectionsAlone is the trap the page-per-section
// split introduced: the Reading form carries no ntfy URL, no compose mode and
// no AI key, so a save that rebuilt Prefs from the form alone would store the
// zero value of every field the posted page doesn't have.
func TestSectionSaveLeavesOtherSectionsAlone(t *testing.T) {
	s := serverWith(t, nil, nil)
	r := settingsRouter(s)

	// Set up two other sections first.
	if rec := postSettings(t, r, url.Values{
		"section":  {"notifications"},
		"ntfy_url": {"https://ntfy.sh/mimux-test"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("notifications save = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postSettings(t, r, url.Values{
		"section":      {"composing"},
		"compose_mode": {"markdown"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("composing save = %d: %s", rec.Code, rec.Body.String())
	}

	// Now save Reading, which posts neither of those fields.
	if rec := postSettings(t, r, url.Values{
		"section":         {"reading"},
		"mark_read_delay": {"7"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("reading save = %d: %s", rec.Code, rec.Body.String())
	}
	p := s.store.GetPrefs()
	if p.MarkReadDelay != 7 {
		t.Errorf("MarkReadDelay = %d, want the value just saved", p.MarkReadDelay)
	}
	if p.NtfyURL != "https://ntfy.sh/mimux-test" {
		t.Errorf("NtfyURL = %q, wiped by a save from another section", p.NtfyURL)
	}
	if p.ComposeMode != "markdown" {
		t.Errorf("ComposeMode = %q, wiped by a save from another section", p.ComposeMode)
	}
}

// TestUnknownSettingsSection: a made-up section is a 404, not a page that
// renders nothing, and a made-up section on the save path is refused rather
// than silently applying every section.
func TestUnknownSettingsSection(t *testing.T) {
	s := serverWith(t, nil, nil)
	r := settingsRouter(s)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /settings/nope = %d, want 404", rec.Code)
	}
	if rec := postSettings(t, r, url.Values{"section": {"nope"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("save with an unknown section = %d, want 400", rec.Code)
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
	body := renderSection(t, s, "reading")
	for _, want := range []string{
		`name="show_avatar"`, `name="show_favicon"`, `name="hide_avatar_mobile"`,
		`name="avatar_shape"`, `:disabled="!avatarsOn"`, `value="circle"`, `value="rounded"`, `value="square"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}

// TestSyncedFoldersSaveRoundTrip: the Syncing page's per-account folder list
// renders the discovered folders with the current choices ticked, and posting
// it back stores exactly what was ticked. The inbox is checked and disabled, so
// the browser never submits its checkbox — a hidden field carries it instead,
// and the store ORs it back in regardless.
func TestSyncedFoldersSaveRoundTrip(t *testing.T) {
	var inbox, sent, archive int64
	s := serverWith(t, nil, func(st *store.Store) {
		if err := st.UpsertAccount(config.Account{Name: "work", Provider: "gmail", Email: "me@gmail.com", Auth: "password", Password: "x"}); err != nil {
			t.Fatal(err)
		}
		inbox, _ = st.UpsertFolder("work", "INBOX", "inbox", 0)
		sent, _ = st.UpsertFolder("work", "Sent", "sent", 1)
		archive, _ = st.UpsertFolder("work", "Archive", "archive", 3)
	})
	r := settingsRouter(s)

	body := renderSection(t, s, "syncing")
	if !strings.Contains(body, "Folders synced continuously") {
		t.Fatal("the syncing page does not offer the folder list")
	}
	for _, want := range []string{
		`name="sync_folder:work" value="` + itoa(archive) + `"`,
		`name="sync_folder:work" value="` + itoa(sent) + `" checked`,
		`type="hidden" name="sync_folder:work" value="` + itoa(inbox) + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}

	// The user unticks Sent and ticks Archive. The inbox rides the hidden field.
	if rec := postSettings(t, r, url.Values{
		"section":           {"syncing"},
		"sync_folder:work":  {itoa(inbox), itoa(archive)},
		"sync_interval_min": {"5"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("syncing save = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := s.store.SyncedFolders("work")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got {
		names = append(names, f.Name)
	}
	if len(names) != 2 || names[0] != "INBOX" || names[1] != "Archive" {
		t.Errorf("synced set = %v, want [INBOX Archive]", names)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
