// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/store"
)

// lookRouter exposes just the public look endpoints.
func lookRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/icon.svg", s.handleIcon)
	r.Get("/manifest.json", s.handleManifest)
	return r
}

// wellFormedXML fails the test unless b parses as XML.
func wellFormedXML(t *testing.T, b string) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(b))
	for {
		if _, err := d.Token(); err == io.EOF {
			return
		} else if err != nil {
			t.Fatalf("icon is not well-formed XML: %v\n%s", err, b)
		}
	}
}

func get(t *testing.T, h http.Handler, url string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", url, rec.Code)
	}
	return rec, rec.Body.String()
}

// TestIconEndpoint checks the icon renders as valid SVG carrying the stored
// colours and shape, that the maskable variant is full-bleed and shrinks the
// mark into the safe zone, and that "transparent" really drops the background.
func TestIconEndpoint(t *testing.T) {
	s := serverWith(t, nil, func(st *store.Store) {
		c := st.GetAppConfig()
		c.IconBG, c.IconAccent, c.IconLeaf, c.IconShape = "#101010", "#ff0000", "#00ff00", "circle"
		if err := st.SaveAppConfig(c); err != nil {
			t.Fatal(err)
		}
	})
	h := lookRouter(s)

	rec, body := get(t, h, "/icon.svg")
	wellFormedXML(t, body)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q; the URL is versioned, it should be cacheable", cc)
	}
	for _, want := range []string{`fill="#101010"`, `fill="#ff0000"`, `fill="#00ff00"`, `rx="16"`} {
		if !strings.Contains(body, want) {
			t.Errorf("icon missing %q; body:\n%s", want, body)
		}
	}

	_, mask := get(t, h, "/icon.svg?p=maskable")
	wellFormedXML(t, mask)
	if !strings.Contains(mask, "scale(0.72)") || !strings.Contains(mask, `rx="0"`) {
		t.Errorf("maskable variant must be full-bleed and shrink the mark; body:\n%s", mask)
	}

	// The notification badge is a white-on-nothing silhouette: Android derives
	// it from the alpha channel, so a background plate or a coloured mark would
	// come out as a blank blob (see sw.js).
	_, badge := get(t, h, "/icon.svg?p=badge")
	wellFormedXML(t, badge)
	if strings.Contains(badge, "<rect") || strings.Contains(badge, "#101010") ||
		strings.Contains(badge, "#ff0000") || strings.Contains(badge, "#00ff00") ||
		strings.Count(badge, `fill="#fff"`) != 2 {
		t.Errorf("badge must be the bare mark in opaque white; body:\n%s", badge)
	}

	// Query overrides drive the Settings preview, through the same gate.
	_, prev := get(t, h, "/icon.svg?accent=%23123456&bg=transparent&shape=square")
	if !strings.Contains(prev, "#123456") || strings.Contains(prev, "<rect") {
		t.Errorf("preview override not applied; body:\n%s", prev)
	}
	// ...and a hostile one never reaches the output.
	_, evil := get(t, h, `/icon.svg?leaf=%22%2F%3E%3Cscript%3Ealert(1)%3C%2Fscript%3E`)
	if strings.Contains(evil, "script") {
		t.Errorf("unvalidated colour reached the SVG; body:\n%s", evil)
	}
	wellFormedXML(t, evil)
}

// TestManifestVersionedIcon checks the manifest points at the versioned icon
// URL (both purposes) and that changing a colour changes that version.
func TestManifestVersionedIcon(t *testing.T) {
	s := serverWith(t, nil, nil)
	_, body := get(t, lookRouter(s), "/manifest.json")
	ver := s.store.GetAppConfig().IconVersion()
	if !strings.Contains(body, `"src": "/icon.svg?v=`+ver+`"`) {
		t.Errorf("manifest missing versioned icon %q; body:\n%s", ver, body)
	}
	if !strings.Contains(body, `"/icon.svg?v=`+ver+`&p=maskable"`) || !strings.Contains(body, `"maskable"`) {
		t.Errorf("manifest missing maskable variant; body:\n%s", body)
	}

	c := s.store.GetAppConfig()
	c.IconLeaf = "#000000"
	if err := s.store.SaveAppConfig(c); err != nil {
		t.Fatal(err)
	}
	if got := s.store.GetAppConfig().IconVersion(); got == ver {
		t.Errorf("icon version %q didn't change after a colour change", got)
	}
}

// TestAccentReachesCSS checks the chosen accent lands in the rendered <head> as
// an indigo-scale override — both the named presets and a custom hex — and that
// the stock accent emits nothing at all.
func TestAccentReachesCSS(t *testing.T) {
	if got := accentCSS("indigo"); got != "" {
		t.Errorf("stock accent should emit no CSS, got %q", got)
	}
	violet, _ := store.AccentSteps("violet")
	if got := string(accentCSS("violet")); !strings.Contains(got, "--color-indigo-500:"+violet[3]) ||
		!strings.Contains(got, `:root[data-theme="light"]{--color-indigo-300:`+violet[5]) {
		t.Errorf("violet accent CSS wrong: %s", got)
	}
	if got := string(accentCSS("#ff8800")); !strings.Contains(got, "--color-indigo-500:#ff8800") ||
		!strings.Contains(got, "color-mix(in oklab,#ff8800") {
		t.Errorf("custom accent CSS wrong: %s", got)
	}
	if got := accentCSS(`</style><script>`); got != "" {
		t.Errorf("junk accent must fall back to stock, got %q", got)
	}

	// End to end: the setting reaches the page head, together with the icon URL.
	s := serverWith(t, nil, func(st *store.Store) {
		c := st.GetAppConfig()
		c.Accent = "emerald"
		if err := st.SaveAppConfig(c); err != nil {
			t.Fatal(err)
		}
	})
	rec := httptest.NewRecorder()
	s.render(rec, "login", map[string]any{})
	body := rec.Body.String()
	emerald, _ := store.AccentSteps("emerald")
	if !strings.Contains(body, "--color-indigo-500:"+emerald[3]) {
		t.Errorf("accent CSS missing from <head>; body:\n%s", body)
	}
	if !strings.Contains(body, `href="/icon.svg?v=`) {
		t.Errorf("page head doesn't link the versioned icon; body:\n%s", body)
	}

	// The Settings → Appearance controls must render too — html/template's CSS
	// filter is happy to blank out a swatch colour it doesn't like.
	set := renderSection(t, s, "appearance")
	for _, want := range []string{`name="ui_accent"`, `name="icon_bg"`, `name="icon_shape"`,
		"background-color: " + emerald[3], `:src="iconSrc"`} {
		if !strings.Contains(set, want) {
			t.Errorf("settings Appearance section missing %q", want)
		}
	}
	if strings.Contains(set, "ZgotmplZ") {
		t.Errorf("a value was blanked by html/template's contextual escaper")
	}
}
