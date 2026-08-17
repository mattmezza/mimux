// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"fmt"
	"html/template"
	"net/http"
	svgtmpl "text/template"

	"github.com/mattmezza/mimux/internal/store"
)

// --- app icon -------------------------------------------------------------

// The mark: an envelope whose flap is a V cut clean out of the body, with one
// leaf sprouting up through the opening. Two fills, one silhouette — the old
// icon had a border, a stem and two leaves as well, which at 16px collapsed
// into mush. Keeping the flap as a hole (not a stroke) means it still reads on
// a transparent background and in monochrome.
// NOTE: the geometry is two literal path strings, not a shape model — only
// the colours are parameterised.
var iconTmpl = svgtmpl.Must(svgtmpl.New("icon").Parse(
	`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32">` +
		`{{if .BG}}<rect width="32" height="32" rx="{{.RX}}" fill="{{.BG}}"/>{{end}}` +
		`<g transform="translate(16 16) scale({{.Scale}}) translate(-16 -16)">` +
		`<path fill="{{.Accent}}" fill-rule="evenodd" d="M7 13h18a3 3 0 0 1 3 3v9a3 3 0 0 1-3 3H7a3 3 0 0 1-3-3v-9a3 3 0 0 1 3-3ZM7 13l9 8.5 9-8.5Z"/>` +
		`<path fill="{{.Leaf}}" d="M16 17c-1.2-6.8 2.4-12.6 9-12.6.9 7-3 12.6-9 12.6Z"/>` +
		`</g></svg>`))

// iconView is the whole parameter surface of the mark.
type iconView struct{ BG, Accent, Leaf, RX, Scale string }

// iconRX maps the shape choice onto the background rect's corner radius.
func iconRX(shape string) string {
	switch shape {
	case "square":
		return "0"
	case "circle":
		return "16"
	default:
		return "7"
	}
}

// iconMark is the mark colour: the explicit choice, else the app accent's 500
// step — picking an accent gives the icon a sensible default, and the colour
// input in Settings stays independently overridable from there.
func iconMark(c store.AppConfig) string {
	if c.IconAccent != "" {
		return c.IconAccent
	}
	if s, ok := store.AccentSteps(c.Accent); ok {
		return s[3]
	}
	return store.ValidHexColor(c.Accent, "#6366f1")
}

// handleIcon renders the app icon as SVG from the stored colours. The URL
// carries ?v=<IconVersion> so it can be cached forever and still change the
// instant the user picks new colours. ?p=maskable renders the Android
// adaptive-icon variant, and the bg/accent/leaf/shape query overrides drive the
// live preview in Settings (same validation gate as the stored values).
func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	c := s.store.GetAppConfig()
	q := r.URL.Query()
	v := iconView{
		BG:     store.ValidIconBG(q.Get("bg"), c.IconBG),
		Accent: store.ValidHexColor(q.Get("accent"), iconMark(c)),
		Leaf:   store.ValidHexColor(q.Get("leaf"), c.IconLeaf),
		Scale:  "1",
	}
	shape := c.IconShape
	if q.Get("shape") != "" {
		shape = q.Get("shape")
	}
	v.RX = iconRX(shape)
	if v.BG == "transparent" {
		v.BG = ""
	}
	switch q.Get("p") {
	case "maskable":
		// Launchers crop to their own circle/squircle and paint the edge
		// themselves, so this variant is full-bleed and shrinks the mark inside
		// the 40%-radius safe zone (the mark's corners sit ~17 units from
		// centre in a 32-unit box; 0.72 puts them at ~12.2, clear of 12.8).
		v.RX, v.Scale = "0", "0.72"
		if v.BG == "" {
			v.BG = "#18181b"
		}
	case "badge":
		// The notification badge (Android's status-bar icon). Android throws the
		// colours away and derives the mark from the *alpha channel* alone, so
		// anything sitting on a background plate — like the app icon — arrives as
		// a featureless rounded blob. Same mark, opaque white, nothing behind it:
		// the flap stays a hole and the leaf stays a notch, so it survives.
		// Colour-independent by construction, so the immutable cache below is
		// still honest without a ?v= on the URL.
		v.BG, v.Accent, v.Leaf = "", "#fff", "#fff"
	}
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_ = iconTmpl.Execute(w, v)
}

// iconURL is the versioned icon path handed to the templates and the manifest.
// Versioning by query is what invalidates both the browser's (very sticky)
// favicon cache and the service worker's runtime cache, which keys on the full
// URL — the old entry is simply never requested again.
func iconURL(c store.AppConfig, purpose string) string {
	u := "/icon.svg?v=" + c.IconVersion()
	if purpose != "" {
		u += "&p=" + purpose
	}
	return u
}

// manifestTmpl is the PWA manifest, emitted from Go so the icon entries carry
// the versioned URL. The SVG entries are the ones the user's colours actually
// reach; the PNGs stay listed, in the default palette, for installers that
// won't rasterize SVG (iOS, and Chrome's WebAPK minting on older Android).
// Settings says so out loud rather than pretending otherwise.
var manifestTmpl = svgtmpl.Must(svgtmpl.New("manifest").Parse(`{
  "name": "mimux",
  "short_name": "mimux",
  "description": "Self-hosted, privacy-first email client",
  "start_url": "/",
  "display": "standalone",
  "background_color": "#18181b",
  "theme_color": "#18181b",
  "icons": [
    { "src": "{{.Icon}}", "sizes": "any", "type": "image/svg+xml", "purpose": "any" },
    { "src": "{{.Maskable}}", "sizes": "any", "type": "image/svg+xml", "purpose": "maskable" },
    { "src": "/static/icons/icon-192.png", "sizes": "192x192", "type": "image/png" },
    { "src": "/static/icons/icon-512.png", "sizes": "512x512", "type": "image/png" }
  ]
}
`))

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	c := s.store.GetAppConfig()
	w.Header().Set("Content-Type", "application/manifest+json")
	_ = manifestTmpl.Execute(w, map[string]string{
		"Icon":     iconURL(c, ""),
		"Maskable": iconURL(c, "maskable"),
	})
}

// --- app accent -----------------------------------------------------------

// accentCSS is the <style> body that repaints the app's accent, empty for the
// stock indigo. Tailwind v4 compiles colour utilities to var(--color-...), so
// redefining the indigo scale recolours every button, ring, link and active
// state without touching a class — exactly the trick app.css already uses to
// flip the zinc scale for the light theme. Emitted in <head> after the
// stylesheet (equal specificity, later wins) so there is no flash.
func accentCSS(accent string) template.CSS {
	accent = store.ValidAccent(accent, "indigo")
	if accent == "indigo" {
		return ""
	}
	steps, ok := store.AccentSteps(accent)
	if !ok {
		// Custom colour: derive the ramp with native color-mix instead of doing
		// palette math in Go.
		// NOTE: a mix ramp, not a perceptual palette — pick a preset if a
		// hand-tuned scale ever matters.
		c := accent
		mix := func(pct int, with string) string {
			return fmt.Sprintf("color-mix(in oklab,%s %d%%,%s)", c, pct, with)
		}
		steps = [8]string{mix(35, "white"), mix(55, "white"), mix(78, "white"), c,
			mix(82, "black"), mix(65, "black"), mix(32, "black"), mix(20, "black")}
	}
	// The light theme darkens the 300/400 steps because they're used as text on
	// white (see app.css); mirror that with this accent's own 700/600 so links
	// and active tabs stay readable there too.
	//nolint:gosec // G203: accent passed through ValidAccent above, so it is
	// either a preset id or a "#rrggbb" literal — never caller-controlled text.
	return template.CSS(fmt.Sprintf(
		`:root{--color-indigo-200:%s;--color-indigo-300:%s;--color-indigo-400:%s;`+
			`--color-indigo-500:%s;--color-indigo-600:%s;--color-indigo-900:%s;--color-indigo-950:%s}`+
			`:root[data-theme="light"]{--color-indigo-300:%s;--color-indigo-400:%s}`,
		steps[0], steps[1], steps[2], steps[3], steps[4], steps[6], steps[7],
		steps[5], steps[4]))
}

// appearanceData is the per-render icon/accent payload every page needs in
// <head>. One settings read per page render — a single-user app on local
// SQLite won't notice.
func (s *Server) appearanceData(data map[string]any) {
	c := s.store.GetAppConfig()
	data["IconURL"] = iconURL(c, "")
	data["AccentCSS"] = accentCSS(c.Accent)
}

// iconShapes are the corner-rounding choices, for the Settings select.
var iconShapes = []struct{ ID, Label string }{
	{"rounded", "Rounded"},
	{"square", "Square"},
	{"circle", "Circle"},
}
