// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"io"
	"mime/quotedprintable"
	"net/textproto"
	"net/url"
	"strings"
	"sync"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // register non-UTF-8 charsets
	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html"

	"github.com/mattmezza/mimux/internal/store"
)

// messageBody is a parsed, decoded email body ready for on-demand rendering.
type messageBody struct {
	htmlContent string
	textContent string
	inline      map[string]inlinePart // keyed by lowercased Content-ID
	// calendar holds the raw bytes of the first text/calendar part (or .ics
	// attachment), so the invite card is parsed from the cached body with no
	// extra IMAP fetch. nil when the message carries no calendar payload.
	calendar       []byte
	calendarInline bool // true once an inline text/calendar was captured (beats a later .ics)
	// listUnsubscribe/listUnsubscribePost carry the raw RFC 2369 / RFC 8058
	// headers (unparsed) so ParseListUnsubscribe can run at render time.
	listUnsubscribe     string
	listUnsubscribePost string
	// headers is the message's whole header block, verbatim (see headerBlock).
	// The warmer's full fetch fills it for free on the inbox and Drafts, so most
	// messages answer Headers with no extra IMAP at all.
	headers      string
	articleMu    sync.Mutex
	articleText  string
	articleReady bool
}

// bodyDTO is the exported, gob-encodable form of messageBody used to persist a
// parsed body to SQLite. Note it carries only text/HTML and inline cid: images —
// attachment parts are already discarded by parseBody — so caching this never
// stores attachment bytes.
type bodyDTO struct {
	HTML                string
	Text                string
	Inline              map[string]inlineDTO
	Calendar            []byte
	ListUnsubscribe     string
	ListUnsubscribePost string
	Headers             string
	ArticleText         string
	ArticleReady        bool
}

type inlineDTO struct {
	Mime string
	Data []byte
}

// encodeBody gob-encodes a parsed body for storage.
func encodeBody(b *messageBody) ([]byte, error) {
	dto := bodyDTO{
		HTML: b.htmlContent, Text: b.textContent, Inline: map[string]inlineDTO{},
		Calendar:        b.calendar,
		ListUnsubscribe: b.listUnsubscribe, ListUnsubscribePost: b.listUnsubscribePost,
		Headers:     b.headers,
		ArticleText: b.articleText, ArticleReady: b.articleReady,
	}
	for k, v := range b.inline {
		dto.Inline[k] = inlineDTO{Mime: v.mime, Data: v.data}
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(dto); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeBody reverses encodeBody.
func decodeBody(blob []byte) (*messageBody, error) {
	var dto bodyDTO
	if err := gob.NewDecoder(bytes.NewReader(blob)).Decode(&dto); err != nil {
		return nil, err
	}
	b := &messageBody{
		htmlContent: dto.HTML, textContent: dto.Text, inline: map[string]inlinePart{},
		calendar:        dto.Calendar,
		listUnsubscribe: dto.ListUnsubscribe, listUnsubscribePost: dto.ListUnsubscribePost,
		headers:     dto.Headers,
		articleText: dto.ArticleText, articleReady: dto.ArticleReady,
	}
	for k, v := range dto.Inline {
		b.inline[k] = inlinePart{mime: v.Mime, data: v.Data}
	}
	return b, nil
}

// render produces the final sanitized HTML document for the reading-pane iframe.
func (b *messageBody) render(allowExternal bool) (out string, blockedExternal bool) {
	htmlSrc := b.htmlContent
	// Some senders ship full HTML under Content-Type: text/plain (or with no
	// type). Render it as HTML — through the same sanitizer + CSP — instead of
	// escaping the tags into a <pre>.
	if strings.TrimSpace(htmlSrc) == "" && looksHTML(b.textContent) {
		htmlSrc = b.textContent
	}
	if strings.TrimSpace(htmlSrc) != "" {
		safe, blocked := sanitizeHTML(htmlSrc, b.inline, allowExternal)
		return renderBodyDocument(safe, false), blocked
	}
	return renderBodyDocument(b.textContent, true), false
}

// PlainText returns the message's readable text, for feeding to something that
// can't read markup (the AI summary). It comes off the same cached parsed body
// as the reading pane (LRU → SQLite → IMAP), preferring the text/plain part and
// falling back to the visible text of the HTML one.
func (m *Manager) PlainText(ctx context.Context, msg *store.Message) (string, error) {
	b, err := m.parsedBody(ctx, msg, false)
	if err != nil {
		return "", err
	}
	if t := strings.TrimSpace(b.textContent); t != "" && !looksHTML(t) {
		return t, nil
	}
	src := b.htmlContent
	if strings.TrimSpace(src) == "" {
		src = b.textContent
	}
	return collapseWS(stripHTML(src)), nil
}

// ArticleText returns summary-grade message text. A substantive plain-text
// alternative remains authoritative; HTML-only newsletters are distilled to
// their main content so navigation and footer chrome do not consume the model
// budget. The result (including a fallback) is persisted with the parsed body.
func (m *Manager) ArticleText(ctx context.Context, msg *store.Message) (string, error) {
	b, err := m.parsedBody(ctx, msg, false)
	if err != nil {
		return "", err
	}
	b.articleMu.Lock()
	defer b.articleMu.Unlock()
	if b.articleReady {
		return b.articleText, nil
	}

	plain := strings.TrimSpace(b.textContent)
	htmlSrc := strings.TrimSpace(b.htmlContent)
	if !looksHTML(plain) && usefulPlainAlternative(plain) {
		b.articleText = plain
	} else if htmlSrc != "" || looksHTML(plain) {
		if htmlSrc == "" {
			htmlSrc = plain
		}
		base, _ := url.Parse("https://email.invalid/")
		if article, extractErr := readability.FromReader(strings.NewReader(htmlSrc), base); extractErr == nil {
			b.articleText = collapseWS(article.TextContent)
		}
		if len([]rune(b.articleText)) < 80 {
			b.articleText = collapseWS(stripHTML(htmlSrc))
		}
	} else {
		b.articleText = plain
	}
	b.articleReady = true
	if blob, encodeErr := encodeBody(b); encodeErr == nil {
		_ = m.st.SaveMessageBody(msg.ID, blob)
	}
	return b.articleText, nil
}

func usefulPlainAlternative(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	l := strings.ToLower(collapseWS(s))
	for _, boilerplate := range []string{"view this email in html", "view this message in html", "html-capable email client", "view in your browser"} {
		if strings.Contains(l, boilerplate) && len([]rune(l)) < 500 {
			return false
		}
	}
	return true
}

// QuoteSource returns a message's body in the two forms a reply or forward
// quotes it in: its text (the text/plain part, or the HTML one rendered down to
// text with the line structure kept — unlike PlainText, which collapses
// everything for the summarizer) and its raw HTML part, "" when it had none.
// The caller sanitizes the HTML with SanitizeComposeHTML: it is about to become
// the user's own markup, not something rendered in the reading pane.
//
// Same cached parsed body as everything else (LRU → SQLite → IMAP).
func (m *Manager) QuoteSource(ctx context.Context, msg *store.Message) (text, htmlBody string, err error) {
	b, err := m.parsedBody(ctx, msg, false)
	if err != nil {
		return "", "", err
	}
	text, htmlBody = b.textContent, b.htmlContent
	// Full HTML shipped under text/plain: it is the HTML part, whatever it said.
	if strings.TrimSpace(htmlBody) == "" && looksHTML(text) {
		text, htmlBody = "", text
	}
	if strings.TrimSpace(text) == "" {
		text = htmlToText(htmlBody)
	}
	return strings.TrimSpace(text), htmlBody, nil
}

// Headers returns the message's raw header block and its parsed form.
func (m *Manager) Headers(ctx context.Context, msg *store.Message) (raw string, parsed map[string][]string, err error) {
	b, err := m.parsedBody(ctx, msg, false)
	if err != nil {
		return "", nil, err
	}
	raw = b.headers
	if raw == "" {
		// A blob cached before headers were stored — no real message has none, so
		// empty is an unambiguous sentinel. Patch it with a header-only fetch
		// rather than pulling the whole message back over IMAP.
		hdr, ferr := m.fetchHeaders(ctx, msg)
		if ferr != nil {
			return "", nil, ferr
		}
		raw = string(hdr)
		// A copy, not a write through the pointer: the LRU hands the same
		// *messageBody to everyone, and render reads it concurrently.
		b.articleMu.Lock()
		patched := &messageBody{
			htmlContent: b.htmlContent, textContent: b.textContent, inline: b.inline,
			calendar: b.calendar, calendarInline: b.calendarInline,
			listUnsubscribe: b.listUnsubscribe, listUnsubscribePost: b.listUnsubscribePost,
			headers: raw, articleText: b.articleText, articleReady: b.articleReady,
		}
		b.articleMu.Unlock()
		if blob, err := encodeBody(patched); err == nil {
			_ = m.st.SaveMessageBody(msg.ID, blob) // best-effort cache
		}
		m.bodies.put(msg.ID, patched)
	}
	return raw, parseHeaders(raw), nil
}

// headerBlock returns a message's header block: everything up to and including
// the blank line that ends it. Stored raw rather than as a parsed map because
// parsing is not reversible — folding, field order and repeated Received: lines
// are all lost — and the raw form is what the API hands back.
func headerBlock(raw []byte) string {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return string(raw[:i+4])
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return string(raw[:i+2])
	}
	return string(raw) // no body at all: it is all header
}

// parseHeaders parses a header block into canonicalised keys, keeping repeated
// fields as multiple values. A malformed line costs the fields after it, not
// the whole call — ReadMIMEHeader returns what it managed to read.
func parseHeaders(raw string) map[string][]string {
	r := strings.NewReader(strings.TrimRight(raw, "\r\n") + "\r\n\r\n") // terminator ReadMIMEHeader insists on
	h, _ := textproto.NewReader(bufio.NewReader(r)).ReadMIMEHeader()
	return h
}

// parseBody parses a full RFC 822 message into its text/HTML parts and inline
// (cid) attachments. It is tolerant of malformed parts — a bad part is skipped,
// never fatal.
func parseBody(raw []byte) *messageBody {
	b := &messageBody{inline: map[string]inlinePart{}}
	ent, err := message.Read(bytes.NewReader(raw))
	if ent == nil {
		if err != nil {
			b.textContent = string(raw)
		}
		return b
	}
	b.listUnsubscribe = ent.Header.Get("List-Unsubscribe")
	b.listUnsubscribePost = ent.Header.Get("List-Unsubscribe-Post")
	b.headers = headerBlock(raw)
	_ = ent.Walk(func(_ []int, part *message.Entity, perr error) error {
		if perr != nil && !message.IsUnknownCharset(perr) && !message.IsUnknownEncoding(perr) {
			return nil // skip unreadable part, keep walking siblings
		}
		mt, _, _ := part.Header.ContentType()
		mt = strings.ToLower(mt)
		if strings.HasPrefix(mt, "multipart/") {
			return nil
		}
		data, _ := io.ReadAll(io.LimitReader(part.Body, 25<<20)) // NOTE: 25MB/part cap guards against decompression bombs
		disp, dparams, _ := part.Header.ContentDisposition()
		_, ctparams, _ := part.Header.ContentType()
		cid := strings.Trim(part.Header.Get("Content-ID"), "<> ")
		fname := strings.ToLower(dparams["filename"] + " " + ctparams["name"])
		// Capture a calendar payload for the invite card. Prefer an inline
		// text/calendar (it carries METHOD); an .ics attachment is a fallback
		// only if no inline part was seen. First inline text/calendar wins.
		isICS := mt == "text/calendar" || strings.Contains(fname, ".ics")
		if isICS && bytes.Contains(data, []byte("BEGIN:VCALENDAR")) {
			if mt == "text/calendar" {
				if !b.calendarInline {
					b.calendar, b.calendarInline = data, true
				}
			} else if b.calendar == nil {
				b.calendar = data
			}
		}
		switch {
		case mt == "text/html" && !strings.EqualFold(disp, "attachment"):
			// Keep the first non-empty HTML body only. Concatenating sibling
			// parts (multipart/mixed with several bodies, forwarded messages,
			// digests) glues multiple complete <html> documents together and
			// renders as garbage. A multipart/alternative has just one HTML
			// leaf, so first-wins is also correct there.
			if b.htmlContent == "" {
				b.htmlContent = string(data)
			}
		case (mt == "text/plain" || mt == "") && !strings.EqualFold(disp, "attachment"):
			if b.textContent == "" {
				b.textContent = string(data)
			}
		case cid != "" && strings.HasPrefix(mt, "image/"):
			b.inline[strings.ToLower(cid)] = inlinePart{mime: mt, data: data}
		}
		return nil
	})
	return b
}

// snippetText decodes a fetched body part (best-effort for the given transfer
// encoding), strips HTML if present, and returns a collapsed preview.
func snippetText(rawPart []byte, encoding, mediaType string) string {
	text := string(decodeTransfer(rawPart, encoding))
	if strings.Contains(strings.ToLower(mediaType), "html") || looksHTML(text) {
		text = stripHTML(text)
	}
	return truncate(collapseWS(text), 500)
}

// nestedSnippet extracts a preview from a nested multipart entity returned by
// BODY[1]. When part 1 is itself a multipart, IMAP returns the whole nested
// MIME entity (its own headers and boundary markers), not just the text. Parse
// it and pull the first text/plain (or HTML) leaf, which is what the list
// preview should actually show.
func nestedSnippet(raw []byte) string {
	ent, err := message.Read(bytes.NewReader(raw))
	if ent == nil {
		return ""
	}
	_ = err
	var text, html string
	_ = ent.Walk(func(_ []int, part *message.Entity, perr error) error {
		if perr != nil && !message.IsUnknownCharset(perr) && !message.IsUnknownEncoding(perr) {
			return nil // skip unreadable part, keep walking siblings
		}
		mt, _, _ := part.Header.ContentType()
		mt = strings.ToLower(mt)
		if strings.HasPrefix(mt, "multipart/") {
			return nil
		}
		disp, _, _ := part.Header.ContentDisposition()
		if strings.EqualFold(disp, "attachment") {
			return nil
		}
		data, _ := io.ReadAll(io.LimitReader(part.Body, 25<<20))
		switch {
		case text == "" && (mt == "text/plain" || mt == ""):
			text = string(data)
		case html == "" && mt == "text/html":
			html = string(data)
		}
		return nil
	})
	if strings.TrimSpace(text) != "" {
		return truncate(collapseWS(text), 500)
	}
	if strings.TrimSpace(html) != "" {
		return truncate(collapseWS(stripHTML(html)), 500)
	}
	return ""
}

func decodeTransfer(b []byte, encoding string) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		// Partial fetches can truncate mid-block; decode the largest valid prefix.
		s := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, string(b))
		s = s[:len(s)-len(s)%4]
		out, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return b
		}
		return out
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(b)))
		if err != nil || len(out) == 0 {
			return b
		}
		return out
	default:
		return b
	}
}

func looksHTML(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "<html") || strings.Contains(l, "<body") ||
		strings.Contains(l, "<div") || strings.Contains(l, "<p>") || strings.Contains(l, "<table")
}

// stripHTML returns the visible text of an HTML fragment.
func stripHTML(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var sb strings.Builder
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return sb.String()
		case html.StartTagToken:
			name, _ := z.TagName()
			if n := string(name); n == "script" || n == "style" || n == "head" {
				skip++
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if n := string(name); (n == "script" || n == "style" || n == "head") && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip == 0 {
				sb.Write(z.Text())
				sb.WriteByte(' ')
			}
		}
	}
}

func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
