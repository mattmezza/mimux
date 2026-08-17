package mail

import (
	"strings"
	"testing"
)

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

// multipart/alternative must render the HTML part, not the plain fallback.
func TestParseAlternativePicksHTML(t *testing.T) {
	raw := crlf(`From: a@b.c
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="B"

--B
Content-Type: text/plain; charset=utf-8

plain fallback
--B
Content-Type: text/html; charset=utf-8

<p>rich body</p>
--B--
`)
	b := parseBody([]byte(raw))
	if !strings.Contains(b.htmlContent, "rich body") {
		t.Errorf("html missing: %q", b.htmlContent)
	}
}

// multipart/mixed with two separate HTML bodies must NOT concatenate into one
// broken document with two <html> roots. First body wins.
func TestParseMixedTwoHTMLNoConcat(t *testing.T) {
	raw := crlf(`From: a@b.c
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="B"

--B
Content-Type: text/html; charset=utf-8

<html><body><p>first</p></body></html>
--B
Content-Type: text/html; charset=utf-8

<html><body><p>second</p></body></html>
--B--
`)
	b := parseBody([]byte(raw))
	if strings.Count(strings.ToLower(b.htmlContent), "<html") > 1 {
		t.Errorf("multiple <html> roots concatenated: %q", b.htmlContent)
	}
	if !strings.Contains(b.htmlContent, "first") {
		t.Errorf("primary body lost: %q", b.htmlContent)
	}
}

// HTML delivered under Content-Type: text/plain must render as HTML, not show
// raw escaped tags in a <pre>.
func TestParseHTMLAsPlainRendersHTML(t *testing.T) {
	raw := crlf(`From: a@b.c
Content-Type: text/plain; charset=utf-8

<html><body><h1>Hello</h1></body></html>
`)
	out, _ := parseBody([]byte(raw)).render(false)
	if strings.Contains(out, "&lt;h1&gt;") {
		t.Errorf("HTML was escaped instead of rendered: %q", out)
	}
	if !strings.Contains(out, "<h1>Hello</h1>") {
		t.Errorf("expected rendered heading: %q", out)
	}
}

// Genuine plain text must still be escaped into a <pre>, not treated as HTML.
func TestParsePlainStaysPlain(t *testing.T) {
	raw := crlf(`From: a@b.c
Content-Type: text/plain; charset=utf-8

Hello world, 3 < 5 and 5 > 3.
`)
	out, _ := parseBody([]byte(raw)).render(false)
	if !strings.Contains(out, `class="mimux-plain"`) {
		t.Errorf("plain text should render in a <pre>: %q", out)
	}
}

// Non-UTF-8 charsets (registered via the blank charset import) must decode.
func TestParseLatin1Decodes(t *testing.T) {
	raw := "From: a@b.c\r\nContent-Type: text/plain; charset=iso-8859-1\r\n\r\nCaf\xe9 time\r\n"
	b := parseBody([]byte(raw))
	if !strings.Contains(b.textContent, "Café") {
		t.Errorf("iso-8859-1 not decoded: %q", b.textContent)
	}
}
