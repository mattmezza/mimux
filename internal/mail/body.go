package mail

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // register non-UTF-8 charsets
	"golang.org/x/net/html"
)

// messageBody is a parsed, decoded email body ready for on-demand rendering.
type messageBody struct {
	htmlContent string
	textContent string
	inline      map[string]inlinePart // keyed by lowercased Content-ID
}

// render produces the final sanitized HTML document for the reading-pane iframe.
func (b *messageBody) render(allowExternal bool) (out string, blockedExternal bool) {
	if strings.TrimSpace(b.htmlContent) != "" {
		safe, blocked := sanitizeHTML(b.htmlContent, b.inline, allowExternal)
		return renderBodyDocument(safe, false), blocked
	}
	return renderBodyDocument(b.textContent, true), false
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
	_ = ent.Walk(func(_ []int, part *message.Entity, perr error) error {
		if perr != nil && !message.IsUnknownCharset(perr) && !message.IsUnknownEncoding(perr) {
			return nil // skip unreadable part, keep walking siblings
		}
		mt, _, _ := part.Header.ContentType()
		mt = strings.ToLower(mt)
		if strings.HasPrefix(mt, "multipart/") {
			return nil
		}
		data, _ := io.ReadAll(io.LimitReader(part.Body, 25<<20)) // ponytail: 25MB/part cap guards against decompression bombs
		disp, _, _ := part.Header.ContentDisposition()
		cid := strings.Trim(part.Header.Get("Content-ID"), "<> ")
		switch {
		case mt == "text/html" && !strings.EqualFold(disp, "attachment"):
			b.htmlContent += string(data)
		case (mt == "text/plain" || mt == "") && !strings.EqualFold(disp, "attachment"):
			b.textContent += string(data)
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
