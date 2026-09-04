// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
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

func TestArticleTextDistillsNewsletterAndPersists(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	m := NewManager(&config.Config{}, st)
	folderID, err := st.UpsertFolder("A", "INBOX", "inbox", 0)
	if err != nil {
		t.Fatal(err)
	}
	msg := &store.Message{Account: "A", FolderID: folderID, UID: 1, MessageID: "newsletter@example.com"}
	if err := st.UpsertMessage(msg); err != nil {
		t.Fatal(err)
	}
	stored, err := st.ListMessages(folderID, 1)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored message: %v, %v", stored, err)
	}
	msg = &stored[0]
	b := &messageBody{
		textContent: "Please view this email in HTML in your browser.",
		htmlContent: `<html><body><nav>` + strings.Repeat("Menu Link ", 100) + `</nav><main><h1>Quarterly launch</h1><p>The launch is scheduled for Friday. Alice owns the rollout checklist and Bob will approve the budget.</p><p>This is the second substantial paragraph explaining the customer migration and the deadline.</p></main><footer>` + strings.Repeat("Social Legal ", 100) + `</footer></body></html>`,
		inline:      map[string]inlinePart{},
	}
	m.bodies.put(msg.ID, b)
	got, err := m.ArticleText(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Quarterly launch", "scheduled for Friday", "Alice owns"} {
		if !strings.Contains(got, want) {
			t.Errorf("ArticleText missing %q: %q", want, got)
		}
	}
	if strings.Count(got, "Menu Link") > 5 || strings.Count(got, "Social Legal") > 5 {
		t.Errorf("newsletter chrome was not distilled: %q", got)
	}
	blob, ok, err := st.GetMessageBody(msg.ID)
	if err != nil || !ok {
		t.Fatalf("persisted body: ok=%v err=%v", ok, err)
	}
	persisted, err := decodeBody(blob)
	if err != nil || !persisted.articleReady || persisted.articleText != got {
		t.Fatalf("persisted article = ready:%v text:%q err:%v", persisted.articleReady, persisted.articleText, err)
	}
}

func TestArticleTextKeepsRealPlainText(t *testing.T) {
	plain := strings.Repeat("A concise but genuine plain text update. ", 8)
	if !usefulPlainAlternative(plain) {
		t.Fatal("substantive plain text rejected")
	}
	if usefulPlainAlternative("Please view this email in HTML in your browser.") {
		t.Fatal("HTML placeholder accepted")
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
