// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	emmail "github.com/emersion/go-message/mail"

	"github.com/mattmezza/mimux/internal/config"
)

func TestPrefixSubject(t *testing.T) {
	cases := []struct {
		kind, in, want string
	}{
		{"reply", "Hello", "Re: Hello"},
		{"reply", "Re: Hello", "Re: Hello"}, // no stacking
		{"reply", "re: hello", "re: hello"}, // already prefixed, case-insensitive
		{"reply_all", "Hello", "Re: Hello"},
		{"forward", "Hello", "Fwd: Hello"},
		{"forward", "Fwd: Hello", "Fwd: Hello"},
		{"new", "Hello", "Hello"},
	}
	for _, c := range cases {
		if got := PrefixSubject(c.kind, c.in); got != c.want {
			t.Errorf("PrefixSubject(%q, %q) = %q, want %q", c.kind, c.in, got, c.want)
		}
	}
}

func TestQuoteBody(t *testing.T) {
	date := time.Date(2026, 7, 20, 10, 30, 0, 0, time.UTC)
	got := QuoteBody(date, "Alice <alice@example.com>", "line one\nline two")
	want := "On Mon, 20 Jul 2026 10:30, Alice <alice@example.com> wrote:\n> line one\n> line two\n"
	if got != want {
		t.Errorf("QuoteBody =\n%q\nwant\n%q", got, want)
	}
}

// TestQuoteOriginal covers the reply prefill in every compose mode and at
// every setting: the quote a reply opens with is the one thing the user sees
// before they type a word.
func TestQuoteOriginal(t *testing.T) {
	date := time.Date(2026, 7, 20, 10, 30, 0, 0, time.UTC)
	const from = "Alice <alice@example.com>"
	const text = "line one\nline two\nline three"
	const htmlBody = `<p>line <b>one</b></p><script>alert(1)</script>`
	const attr = "On Mon, 20 Jul 2026 10:30, Alice <alice@example.com> wrote:"

	t.Run("plain quotes every line", func(t *testing.T) {
		got := QuoteOriginal("plain", "all", 10, date, from, text, htmlBody)
		want := "\n\n" + attr + "\n> line one\n> line two\n> line three\n"
		if got != want {
			t.Errorf("got\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("markdown separates the attribution from the blockquote", func(t *testing.T) {
		got := QuoteOriginal("markdown", "all", 10, date, from, text, htmlBody)
		want := "\n\n" + attr + "\n\n> line one\n> line two\n> line three\n"
		if got != want {
			t.Errorf("got\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("html blockquotes the sanitized original", func(t *testing.T) {
		got := QuoteOriginal("html", "all", 10, date, from, text, htmlBody)
		if !strings.HasPrefix(got, "<p><br></p><blockquote>") || !strings.HasSuffix(got, "</blockquote>") {
			t.Fatalf("not a top-posted blockquote: %q", got)
		}
		if !strings.Contains(got, "line <b>one</b>") {
			t.Errorf("original markup missing: %q", got)
		}
		if strings.Contains(got, "<script") {
			t.Errorf("compose sanitizer did not run: %q", got)
		}
	})

	t.Run("html falls back to the text alternative", func(t *testing.T) {
		got := QuoteOriginal("html", "all", 10, date, from, text, "")
		if !strings.Contains(got, "line one<br>line two") {
			t.Errorf("text fallback missing: %q", got)
		}
	})

	t.Run("first N lines truncates and marks it, html included", func(t *testing.T) {
		for _, mode := range []string{"plain", "markdown", "html"} {
			got := QuoteOriginal(mode, "lines", 2, date, from, text, htmlBody)
			if strings.Contains(got, "line three") {
				t.Errorf("%s: quoted past the line budget: %q", mode, got)
			}
			if !strings.Contains(got, "line two") || !strings.Contains(got, truncMarker) {
				t.Errorf("%s: want two lines and a truncation marker: %q", mode, got)
			}
			// The markup is dropped when truncating — cutting a tree at line 2
			// would leave unbalanced tags.
			if mode == "html" && strings.Contains(got, "<b>") {
				t.Errorf("html truncation kept the markup: %q", got)
			}
		}
	})

	t.Run("a short original is not marked as truncated", func(t *testing.T) {
		got := QuoteOriginal("plain", "lines", 10, date, from, text, "")
		if strings.Contains(got, truncMarker) {
			t.Errorf("marked short body as truncated: %q", got)
		}
	})

	t.Run("none quotes nothing at all", func(t *testing.T) {
		for _, mode := range []string{"plain", "markdown", "html"} {
			if got := QuoteOriginal(mode, "none", 10, date, from, text, htmlBody); got != "" {
				t.Errorf("%s: want empty body, got %q", mode, got)
			}
		}
	})
}

func TestReplyRecipients(t *testing.T) {
	got := ReplyRecipients("me@example.com", "Alice <alice@example.com>")
	want := []string{"Alice <alice@example.com>"}
	if !slices.Equal(got, want) {
		t.Errorf("ReplyRecipients = %v, want %v", got, want)
	}
	// Replying to yourself (e.g. a message you sent) drops out entirely.
	if got := ReplyRecipients("me@example.com", "me@example.com"); len(got) != 0 {
		t.Errorf("ReplyRecipients(self) = %v, want empty", got)
	}
}

func TestReplyAllRecipients(t *testing.T) {
	cases := []struct {
		name           string
		self           string
		from, to, cc   []string
		wantTo, wantCc []string
	}{
		{
			name: "basic",
			self: "me@example.com",
			from: []string{"alice@example.com"}, to: []string{"me@example.com", "bob@example.com"}, cc: []string{"carol@example.com"},
			wantTo: []string{"alice@example.com", "bob@example.com"}, wantCc: []string{"carol@example.com"},
		},
		{
			name: "dedup between to and cc",
			self: "me@example.com",
			from: []string{"alice@example.com"}, to: []string{"me@example.com"}, cc: []string{"alice@example.com", "dave@example.com"},
			wantTo: []string{"alice@example.com"}, wantCc: []string{"dave@example.com"},
		},
		{
			name: "self in from is dropped",
			self: "me@example.com",
			from: []string{"me@example.com"}, to: []string{"bob@example.com"}, cc: nil,
			wantTo: []string{"bob@example.com"}, wantCc: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			to, cc := ReplyAllRecipients(c.self, c.from, c.to, c.cc)
			if !slices.Equal(to, c.wantTo) {
				t.Errorf("to = %v, want %v", to, c.wantTo)
			}
			if !slices.Equal(cc, c.wantCc) {
				t.Errorf("cc = %v, want %v", cc, c.wantCc)
			}
		})
	}
}

func TestComputeReferences(t *testing.T) {
	cases := []struct {
		name, origRefs, origMessageID, want string
	}{
		{"no prior refs", "", "abc@example.com", "<abc@example.com>"},
		{"appends to existing chain", "<a@x> <b@x>", "c@x", "<a@x> <b@x> <c@x>"},
		{"dedup", "<a@x> <c@x>", "c@x", "<a@x> <c@x>"},
		{"no message id", "<a@x>", "", "<a@x>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ComputeReferences(c.origRefs, c.origMessageID); got != c.want {
				t.Errorf("ComputeReferences(%q, %q) = %q, want %q", c.origRefs, c.origMessageID, got, c.want)
			}
		})
	}
}

func TestSplitAddrList(t *testing.T) {
	got := SplitAddrList(" a@x , b@x ,, ")
	want := []string{"a@x", "b@x"}
	if !slices.Equal(got, want) {
		t.Errorf("SplitAddrList = %v, want %v", got, want)
	}
	if got := SplitAddrList("  "); got != nil {
		t.Errorf("SplitAddrList(blank) = %v, want nil", got)
	}
}

// TestBuildMessageAlias verifies send-as: a non-empty ComposeInput.From
// overrides the account's primary address in both the From header and the
// Message-ID host, while keeping the account's display name.
func TestBuildMessageAlias(t *testing.T) {
	cfg := config.Account{Name: "Work", Email: "me@example.com"}
	in := ComposeInput{To: []string{"a@x.com"}, Subject: "hi", Body: "yo", From: "me@alias.org"}
	raw, msgID, err := BuildMessage(cfg, in, time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(msgID, "@alias.org") {
		t.Errorf("Message-ID host = %q, want alias domain", msgID)
	}
	r, err := emmail.CreateReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	from, _ := r.Header.AddressList("From")
	if len(from) != 1 || from[0].Address != "me@alias.org" || from[0].Name != "Work" {
		t.Errorf("From = %+v, want me@alias.org / Work", from)
	}
}

// TestBuildMessage builds a message then parses it back with go-message,
// asserting the headers/threading/subject came through correctly.
func TestBuildMessage(t *testing.T) {
	cfg := config.Account{Name: "Work", Email: "me@example.com"}
	in := ComposeInput{
		To:         []string{"Alice <alice@example.com>"},
		Cc:         []string{"bob@example.com"},
		Bcc:        []string{"hidden@example.com"},
		Subject:    "Re: Project status",
		Body:       "Sounds good.\nSee you then.",
		InReplyTo:  "orig@example.com",
		References: "<root@example.com> <orig@example.com>",
	}
	now := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)

	raw, msgID, err := BuildMessage(cfg, in, now)
	if err != nil {
		t.Fatal(err)
	}
	if msgID == "" {
		t.Fatal("expected a generated Message-ID")
	}
	if !strings.HasSuffix(msgID, "@example.com") {
		t.Errorf("Message-ID host = %q, want it to use the account's domain", msgID)
	}

	r, err := emmail.CreateReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse built message: %v", err)
	}
	from, _ := r.Header.AddressList("From")
	if len(from) != 1 || from[0].Address != "me@example.com" || from[0].Name != "Work" {
		t.Errorf("From = %+v", from)
	}
	to, _ := r.Header.AddressList("To")
	if len(to) != 1 || to[0].Address != "alice@example.com" {
		t.Errorf("To = %+v", to)
	}
	cc, _ := r.Header.AddressList("Cc")
	if len(cc) != 1 || cc[0].Address != "bob@example.com" {
		t.Errorf("Cc = %+v", cc)
	}
	// Bcc must never appear in the header.
	if v := r.Header.Get("Bcc"); v != "" {
		t.Errorf("Bcc leaked into header: %q", v)
	}
	if subj, _ := r.Header.Subject(); subj != "Re: Project status" {
		t.Errorf("Subject = %q", subj)
	}
	if irt, _ := r.Header.MsgIDList("In-Reply-To"); len(irt) != 1 || irt[0] != "orig@example.com" {
		t.Errorf("In-Reply-To = %v", irt)
	}
	if refs, _ := r.Header.MsgIDList("References"); len(refs) != 2 || refs[1] != "orig@example.com" {
		t.Errorf("References = %v", refs)
	}
	if got, _ := r.Header.MessageID(); got != msgID {
		t.Errorf("parsed Message-ID = %q, want %q", got, msgID)
	}
}

// TestBuildMessageAttachments builds a message with two attachments and reads
// it back, asserting the multipart/mixed structure, the text body, and each
// attachment's filename and (base64-round-tripped) bytes.
func TestBuildMessageAttachments(t *testing.T) {
	cfg := config.Account{Name: "Work", Email: "me@example.com"}
	in := ComposeInput{
		To:      []string{"a@x.com"},
		Subject: "with files",
		Body:    "see attached",
		Attachments: []OutAttachment{
			{Filename: "hello.txt", ContentType: "text/plain", Data: []byte("hello world")},
			{Filename: "data.bin", Data: []byte{0x00, 0x01, 0x02, 0xff}}, // no content type -> octet-stream
		},
	}
	raw, _, err := BuildMessage(cfg, in, time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	r, err := emmail.CreateReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var body string
	files := map[string][]byte{}
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch h := p.Header.(type) {
		case *emmail.InlineHeader:
			b, _ := io.ReadAll(p.Body)
			body = strings.TrimSpace(string(b))
		case *emmail.AttachmentHeader:
			name, _ := h.Filename()
			b, _ := io.ReadAll(p.Body)
			files[name] = b
		}
	}
	if body != "see attached" {
		t.Errorf("body = %q", body)
	}
	if got := files["hello.txt"]; string(got) != "hello world" {
		t.Errorf("hello.txt = %q", got)
	}
	if got := files["data.bin"]; !bytes.Equal(got, []byte{0x00, 0x01, 0x02, 0xff}) {
		t.Errorf("data.bin = %v", got)
	}
}

// TestRenderMarkdown checks GFM rendering, that raw HTML is escaped (not passed
// through), and that autolinks/strikethrough work.
func TestRenderMarkdown(t *testing.T) {
	out := RenderMarkdown("# Hi\n\n**bold** and ~~gone~~\n\n- a\n- b\n\n<script>alert(1)</script>")
	for _, want := range []string{"<h1", "<strong>bold</strong>", "<del>gone</del>", "<ul>", "<li>a</li>"} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderMarkdown missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<script>") {
		t.Errorf("raw <script> passed through:\n%s", out)
	}
}

// TestWrapHTMLEmail checks the wrapper ships a full document whose stylesheet is
// scoped to the .mimux-body container so it can't bleed into quoted replies.
func TestWrapHTMLEmail(t *testing.T) {
	out := WrapHTMLEmail("<p>hi</p>")
	for _, want := range []string{"<!DOCTYPE html>", "<style>", ".mimux-body ", `class="mimux-body"`, "<p>hi</p>"} {
		if !strings.Contains(out, want) {
			t.Errorf("WrapHTMLEmail missing %q in:\n%s", want, out)
		}
	}
}

// TestHTMLToText flattens a WYSIWYG fragment to readable plain text with block
// breaks and no tags.
func TestHTMLToText(t *testing.T) {
	got := htmlToText("<p>Hello <b>there</b></p><p>Line<br>two</p>")
	want := "Hello there\nLine\ntwo"
	if got != want {
		t.Errorf("htmlToText = %q, want %q", got, want)
	}
}

// readParts walks a message returning the media types seen and the decoded text
// of the text/plain and text/html inline parts.
func partsOf(t *testing.T, raw []byte) (plain, htmlBody string, cts []string) {
	t.Helper()
	r, err := emmail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ih, ok := p.Header.(*emmail.InlineHeader); ok {
			ct, _, _ := ih.ContentType()
			cts = append(cts, ct)
			b, _ := io.ReadAll(p.Body)
			if ct == "text/html" {
				htmlBody = string(b)
			} else {
				plain = string(b)
			}
		}
	}
	return plain, htmlBody, cts
}

// TestBuildMessageMarkdown asserts a markdown compose becomes multipart/
// alternative carrying the markdown source as text/plain and rendered HTML as
// text/html.
func TestBuildMessageMarkdown(t *testing.T) {
	cfg := config.Account{Name: "Work", Email: "me@example.com"}
	in := ComposeInput{To: []string{"a@x.com"}, Subject: "md", Mode: "markdown", Body: "**hi** there"}
	raw, _, err := BuildMessage(cfg, in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ct := ctypeOf(t, raw); !strings.HasPrefix(ct, "multipart/alternative") {
		t.Errorf("top Content-Type = %q, want multipart/alternative", ct)
	}
	plain, htmlBody, cts := partsOf(t, raw)
	if !slices.Contains(cts, "text/plain") || !slices.Contains(cts, "text/html") {
		t.Errorf("parts = %v, want both text/plain and text/html", cts)
	}
	if strings.TrimSpace(plain) != "**hi** there" {
		t.Errorf("text/plain = %q, want the markdown source", plain)
	}
	if !strings.Contains(htmlBody, "<strong>hi</strong>") {
		t.Errorf("text/html missing rendered markdown:\n%s", htmlBody)
	}
}

// TestBuildMessageHTML asserts a WYSIWYG compose becomes multipart/alternative
// with a stripped text/plain and the wrapped, sanitized HTML.
func TestBuildMessageHTML(t *testing.T) {
	cfg := config.Account{Name: "Work", Email: "me@example.com"}
	in := ComposeInput{To: []string{"a@x.com"}, Subject: "h", Mode: "html",
		Body: `<p>Hi <b>bob</b></p><script>evil()</script>`}
	raw, _, err := BuildMessage(cfg, in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	plain, htmlBody, cts := partsOf(t, raw)
	if !slices.Contains(cts, "text/plain") || !slices.Contains(cts, "text/html") {
		t.Errorf("parts = %v", cts)
	}
	if strings.Contains(htmlBody, "<script>") {
		t.Errorf("script survived sanitization:\n%s", htmlBody)
	}
	if !strings.Contains(htmlBody, "<b>bob</b>") {
		t.Errorf("formatting lost:\n%s", htmlBody)
	}
	if !strings.Contains(plain, "Hi bob") {
		t.Errorf("text/plain = %q, want stripped text", plain)
	}
}

// TestBuildMessagePlainUnchanged guards zero regression: an empty/"plain" mode
// still yields a single text/plain body (no multipart).
func TestBuildMessagePlainUnchanged(t *testing.T) {
	cfg := config.Account{Name: "Work", Email: "me@example.com"}
	in := ComposeInput{To: []string{"a@x.com"}, Subject: "p", Body: "just text"}
	raw, _, err := BuildMessage(cfg, in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ct := ctypeOf(t, raw); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

// TestBuildMessageHTMLAttachments nests the alternative inside multipart/mixed
// alongside the attachment.
func TestBuildMessageHTMLAttachments(t *testing.T) {
	cfg := config.Account{Name: "Work", Email: "me@example.com"}
	in := ComposeInput{To: []string{"a@x.com"}, Subject: "h", Mode: "markdown", Body: "**hi**",
		Attachments: []OutAttachment{{Filename: "a.txt", Data: []byte("x")}}}
	raw, _, err := BuildMessage(cfg, in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ct := ctypeOf(t, raw); !strings.HasPrefix(ct, "multipart/mixed") {
		t.Errorf("top Content-Type = %q, want multipart/mixed", ct)
	}
	// Walk the mixed container: expect a multipart/alternative and an attachment.
	r, err := emmail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var sawHTML, sawAttach bool
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch h := p.Header.(type) {
		case *emmail.InlineHeader:
			if ct, _, _ := h.ContentType(); ct == "text/html" {
				sawHTML = true
			}
		case *emmail.AttachmentHeader:
			if n, _ := h.Filename(); n == "a.txt" {
				sawAttach = true
			}
		}
	}
	if !sawHTML || !sawAttach {
		t.Errorf("sawHTML=%v sawAttach=%v, want both", sawHTML, sawAttach)
	}
}

func ctypeOf(t *testing.T, raw []byte) string {
	t.Helper()
	r, err := emmail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	ct, _, _ := r.Header.ContentType()
	return ct
}

// ConvertBody backs the per-message format switcher in compose: every switch
// has to keep the words, and the two real conversions (markdown->html,
// html->text) have to actually run.
func TestConvertBody(t *testing.T) {
	cases := []struct {
		from, to, in string
		want         []string // substrings the result must contain
		notWant      []string
	}{
		{"plain", "markdown", "hi there", []string{"hi there"}, nil},
		{"markdown", "plain", "**bold** text", []string{"**bold** text"}, nil},
		{"markdown", "html", "**bold** text", []string{"<strong>bold</strong>"}, nil},
		{"plain", "html", "one\ntwo\n\nthree", []string{"<p>one<br>two</p>", "<p>three</p>"}, nil},
		{"plain", "html", "a & <b>", []string{"&amp;", "&lt;b&gt;"}, nil},
		{"html", "plain", "<p>hello <b>world</b></p><p>again</p>", []string{"hello world", "again"}, []string{"<b>", "<p>"}},
		{"html", "markdown", "<p>hello <b>world</b></p>", []string{"hello world"}, []string{"<b>"}},
		{"html", "html", "<p>untouched</p>", []string{"<p>untouched</p>"}, nil},
		{"html", "plain", "", []string{""}, nil},
	}
	for _, c := range cases {
		got := ConvertBody(c.in, c.from, c.to)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("ConvertBody(%q, %s->%s) = %q, want it to contain %q", c.in, c.from, c.to, got, w)
			}
		}
		for _, n := range c.notWant {
			if strings.Contains(got, n) {
				t.Errorf("ConvertBody(%q, %s->%s) = %q, should not contain %q", c.in, c.from, c.to, got, n)
			}
		}
	}
}
