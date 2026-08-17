// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"strings"
	"testing"
)

func TestSnippetPlainText(t *testing.T) {
	got := snippetText([]byte("Hello   world\n\nsecond  line"), "", "text/plain")
	if got != "Hello world second line" {
		t.Errorf("snippet = %q", got)
	}
}

func TestSnippetStripsHTML(t *testing.T) {
	in := []byte(`<html><head><style>.x{color:red}</style></head><body><p>Hi <b>there</b></p><script>evil()</script></body></html>`)
	got := snippetText(in, "", "text/html")
	if strings.Contains(got, "<") || strings.Contains(got, "evil") || strings.Contains(got, "color:red") {
		t.Errorf("html/script/style must be stripped: %q", got)
	}
	if !strings.Contains(got, "Hi") || !strings.Contains(got, "there") {
		t.Errorf("visible text should survive: %q", got)
	}
}

func TestSnippetQuotedPrintable(t *testing.T) {
	got := snippetText([]byte("Caf=C3=A9 time"), "quoted-printable", "text/plain")
	if !strings.Contains(got, "Café") {
		t.Errorf("quoted-printable should decode: %q", got)
	}
}

func TestSnippetBase64(t *testing.T) {
	// base64 of "Hello base64 world"
	got := snippetText([]byte("SGVsbG8gYmFzZTY0IHdvcmxk"), "base64", "text/plain")
	if !strings.Contains(got, "Hello base64 world") {
		t.Errorf("base64 should decode: %q", got)
	}
}

func TestSnippetBase64Truncated(t *testing.T) {
	// A partial fetch can cut base64 mid-block; decode must not panic and should
	// recover the valid prefix.
	got := snippetText([]byte("SGVsbG8gYmFzZTY0IHdvcmxkAAAA=="[:14]), "base64", "text/plain")
	if strings.Contains(got, "<") {
		t.Errorf("unexpected markup: %q", got)
	}
}

func TestSnippetTruncatesLong(t *testing.T) {
	long := strings.Repeat("word ", 300)
	got := snippetText([]byte(long), "", "text/plain")
	if len([]rune(got)) > 501 {
		t.Errorf("snippet not truncated: %d runes", len([]rune(got)))
	}
}

func TestNestedSnippetParsesMultipart(t *testing.T) {
	// BODY[1] on a message whose part 1 is itself a multipart returns the whole
	// nested MIME entity (headers + boundaries), not bare text. The preview must
	// extract the real text instead of leaking boundary/header junk.
	raw := "Content-Type: multipart/alternative; boundary=\"mk3-xyz\"\r\n" +
		"MIME-Version: 1.0\r\n\r\n" +
		"--mk3-xyz\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: 7bit\r\n\r\n" +
		"A new event has been scheduled.\r\n" +
		"--mk3-xyz\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n\r\n" +
		"<html><body><p>A new event has been scheduled.</p></body></html>\r\n" +
		"--mk3-xyz--\r\n"
	got := nestedSnippet([]byte(raw))
	if strings.Contains(got, "boundary") || strings.Contains(got, "Content-Type") || strings.Contains(got, "mk3") {
		t.Errorf("snippet leaked MIME junk: %q", got)
	}
	if !strings.Contains(got, "A new event has been scheduled") {
		t.Errorf("snippet missing real text: %q", got)
	}
}

func TestNestedSnippetPrefersPlainOverHtml(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=\"b\"\r\n\r\n" +
		"--b\r\n" +
		"Content-Type: text/html\r\n\r\n" +
		"<html><body><p>HTML body only</p></body></html>\r\n" +
		"--b\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"Plain body\r\n" +
		"--b--\r\n"
	got := nestedSnippet([]byte(raw))
	if got != "Plain body" {
		t.Errorf("should prefer plain part, got %q", got)
	}
}

func TestNestedSnippetHTMLFallback(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=\"b\"\r\n\r\n" +
		"--b\r\n" +
		"Content-Type: text/html\r\n\r\n" +
		"<html><body><p>Only HTML body here</p></body></html>\r\n" +
		"--b--\r\n"
	got := nestedSnippet([]byte(raw))
	if strings.Contains(got, "<") || strings.Contains(got, "html") {
		t.Errorf("html should be stripped: %q", got)
	}
	if !strings.Contains(got, "Only HTML body here") {
		t.Errorf("expected html text: %q", got)
	}
}
