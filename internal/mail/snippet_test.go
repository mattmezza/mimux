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
