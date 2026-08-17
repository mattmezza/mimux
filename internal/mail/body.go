package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"io"
	"mime/quotedprintable"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // register non-UTF-8 charsets
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
