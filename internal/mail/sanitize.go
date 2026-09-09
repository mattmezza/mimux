// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

// SECURITY-CRITICAL. Untrusted email HTML is sanitized in two passes:
//
//  1. bluemonday reduces the markup to a strict allowlist. This removes the
//     dangerous vectors entirely — <script>/<iframe>/<object>/<embed>/<form>/
//     <base>/<meta>/<svg>, every on* handler, and any href/src whose scheme is
//     not http/https/mailto/cid (so javascript: and data:text/html are gone).
//     Inline CSS is restricted to a fixed set of non-URL properties, so CSS
//     url() exfiltration is structurally impossible.
//  2. A DOM pass over the *already-safe* tree rewrites images: cid: references
//     become inline data: URIs, and remaining external images are stripped to a
//     transparent placeholder with the original stashed in data-ext-src for the
//     "Load external content" button (skipped when allowExternal is set).
//
// The sanitized fragment is served inside an <iframe sandbox> under a strict CSP
// as defence in depth.

// 1x1 transparent GIF — the placeholder for blocked external images.
const blankGIF = "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"

// safeStyleProps are inline CSS properties kept during sanitization. None of
// them accept a url() value, which blocks CSS-based external loads.
var safeStyleProps = []string{
	"color", "background-color", "font", "font-family", "font-size", "font-style",
	"font-weight", "line-height", "letter-spacing", "text-align", "text-decoration",
	"text-transform", "vertical-align", "white-space", "direction",
	"margin", "margin-top", "margin-bottom", "margin-left", "margin-right",
	"padding", "padding-top", "padding-bottom", "padding-left", "padding-right",
	"border", "border-top", "border-bottom", "border-left", "border-right",
	"border-color", "border-width", "border-style", "border-radius", "border-collapse",
	"width", "height", "max-width", "min-width", "display", "float", "clear",
}

var emailPolicy = buildEmailPolicy()

func buildEmailPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()

	// URL schemes: no data:, no javascript:. cid: survives for the DOM pass to
	// resolve into inline data: URIs afterwards.
	p.AllowURLSchemes("http", "https", "mailto", "cid")
	p.RequireParseableURLs(true)
	p.AllowRelativeURLs(false)

	// Links.
	p.AllowAttrs("href").OnElements("a")
	p.RequireNoFollowOnLinks(true)

	// Images (src validated against the schemes above).
	p.AllowAttrs("src", "srcset", "alt", "width", "height").OnElements("img")
	p.AllowAttrs("src", "srcset", "type", "media").OnElements("source")
	p.AllowElements("picture")

	// Text structure and formatting.
	p.AllowElements(
		"p", "div", "span", "br", "hr", "pre", "blockquote",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"b", "strong", "i", "em", "u", "s", "strike", "del", "ins", "sub", "sup", "small", "mark", "code", "kbd",
		"ul", "ol", "li", "dl", "dt", "dd",
		"table", "thead", "tbody", "tfoot", "tr", "td", "th", "caption", "colgroup", "col",
		"figure", "figcaption", "address", "cite", "abbr", "center", "font",
	)

	// Presentational table/font attributes (colors, not URLs).
	p.AllowAttrs("align", "valign", "width", "height", "bgcolor", "border",
		"cellpadding", "cellspacing", "colspan", "rowspan", "nowrap").
		OnElements("table", "tr", "td", "th", "thead", "tbody", "tfoot", "col", "colgroup", "div", "p", "img")
	p.AllowAttrs("color", "face", "size").OnElements("font")
	p.AllowAttrs("dir").Globally()

	// Inline styles, restricted to non-URL properties.
	p.AllowStyles(safeStyleProps...).Globally()

	// Belt and braces: drop the text content of dangerous containers too.
	p.SkipElementsContent("script", "style", "svg", "object", "embed", "form",
		"iframe", "noscript", "head", "title", "template")

	return p
}

// inlinePart is a decoded inline MIME attachment referenced by cid:.
type inlinePart struct {
	mime string
	data []byte
}

// sanitizeHTML runs the two-pass sanitizer. It returns the safe HTML fragment
// and whether any external image was blocked (so the UI can offer to load it).
func sanitizeHTML(raw string, inline map[string]inlinePart, allowExternal bool) (safe string, blockedExternal bool) {
	clean := emailPolicy.Sanitize(raw)

	doc, err := html.Parse(strings.NewReader(clean))
	if err != nil {
		// Parsing bluemonday's own output should never fail; if it does, the
		// escaped fragment is still safe to serve.
		return clean, false
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "img" || n.Data == "source") {
			if rewriteImageNode(n, inline, allowExternal) {
				blockedExternal = true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return clean, blockedExternal
	}
	return buf.String(), blockedExternal
}

// rewriteImageNode resolves cid: sources to data: URIs and neutralizes external
// ones. It returns true if it blocked an external resource.
func rewriteImageNode(n *html.Node, inline map[string]inlinePart, allowExternal bool) bool {
	blocked := false
	var extSrc, extSrcset string
	// Mutate values in place first; appending data-ext-* attrs is deferred until
	// after the loop so it can't reallocate the slice we're ranging over.
	for i := range n.Attr {
		a := &n.Attr[i]
		switch strings.ToLower(a.Key) {
		case "src":
			if v, isCID := resolveCID(a.Val, inline); isCID {
				a.Val = v
			} else if isExternal(a.Val) && !allowExternal {
				extSrc = a.Val
				a.Val = blankGIF
				blocked = true
			}
		case "srcset":
			// srcset only loads images; if anything external remains, blank it.
			if !allowExternal && srcsetHasExternal(a.Val, inline) {
				extSrcset = a.Val
				a.Val = ""
				blocked = true
			}
		}
	}
	if extSrc != "" {
		setAttr(n, "data-ext-src", extSrc)
	}
	if extSrcset != "" {
		setAttr(n, "data-ext-srcset", extSrcset)
	}
	return blocked
}

func resolveCID(src string, inline map[string]inlinePart) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(src), "cid:") {
		return "", false
	}
	id := strings.TrimSpace(src[len("cid:"):])
	id = strings.Trim(id, "<>")
	part, ok := inline[strings.ToLower(id)]
	if !ok {
		// Unknown cid: render nothing rather than a broken external request.
		return blankGIF, true
	}
	return fmt.Sprintf("data:%s;base64,%s", part.mime, base64.StdEncoding.EncodeToString(part.data)), true
}

func isExternal(src string) bool {
	s := strings.ToLower(strings.TrimSpace(src))
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "//")
}

func srcsetHasExternal(srcset string, inline map[string]inlinePart) bool {
	for _, cand := range strings.Split(srcset, ",") {
		fields := strings.Fields(cand)
		if len(fields) == 0 {
			continue
		}
		if _, isCID := resolveCID(fields[0], inline); isCID {
			continue
		}
		if isExternal(fields[0]) {
			return true
		}
	}
	return false
}

func setAttr(n *html.Node, key, val string) {
	for i := range n.Attr {
		if n.Attr[i].Key == key {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

// renderBodyDocument wraps a sanitized fragment (or escaped plain text) in a
// minimal HTML document with dark-mode-aware base styles for the reading-pane
// iframe.
func renderBodyDocument(fragment string, plain bool) string {
	body := fragment
	if plain {
		body = `<pre class="mimux-plain">` + html.EscapeString(fragment) + `</pre>`
	}
	// Email HTML is authored for a light background (like every major webmail
	// client renders it). Forcing the app's dark theme onto it makes messages
	// with hardcoded black text unreadable, so we always render light here and
	// let the reading pane offer a per-message dark toggle (see toggleBodyTheme
	// in app.js, which flips the .mimux-dark class below).
	// base target=_blank: links/buttons in email must open in a new tab, not
	// navigate the sandboxed iframe (which just breaks). Modern browsers imply
	// rel=noopener for target=_blank, so no reverse-tabnabbing exposure.
	// viewport meta: without it the iframe document falls back to a 980px layout
	// viewport on mobile, and pinch-to-zoom anchors at the top-left corner
	// instead of the center of the pinch (issue #52). Applies to plain text too.
	return `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1, user-scalable=yes"><base target="_blank">
<style>
  :root { color-scheme: light; }
  html, body { margin: 0; padding: 12px; }
  body { font: 14px/1.5 -apple-system, system-ui, sans-serif; color: #18181b; background: #fff; word-wrap: break-word; overflow-wrap: break-word; }
  img, table { max-width: 100%; }
  a { color: #4f46e5; }
  .mimux-plain { white-space: pre-wrap; font-family: ui-monospace, monospace; margin: 0; }
  html.mimux-dark, html.mimux-dark body { background: #18181b !important; color: #d4d4d8 !important; }
  html.mimux-dark a { color: #818cf8 !important; }
  /* Thin themed scrollbar, matching the app chrome, on both light and dark. */
  * { scrollbar-width: thin; scrollbar-color: #c4c4cc transparent; }
  ::-webkit-scrollbar { width: 8px; height: 8px; }
  ::-webkit-scrollbar-track, ::-webkit-scrollbar-corner { background: transparent; }
  ::-webkit-scrollbar-thumb { background: #c4c4cc; border-radius: 4px; }
  ::-webkit-scrollbar-thumb:hover { background: #a1a1aa; }
  html.mimux-dark { color-scheme: dark; }
  html.mimux-dark * { scrollbar-color: #3f3f46 transparent; }
  html.mimux-dark ::-webkit-scrollbar-thumb { background: #3f3f46; }
  html.mimux-dark ::-webkit-scrollbar-thumb:hover { background: #52525b; }
</style></head><body>` + body + `</body></html>`
}
