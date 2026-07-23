package mail

import (
	"bytes"
	"html"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	xhtml "golang.org/x/net/html"
)

// mdRenderer is the single markdown->HTML renderer shared by the compose
// preview endpoint and the send path, so what you preview is what you send.
// GFM on (tables, strikethrough, autolinks, task lists); raw HTML passthrough
// stays disabled (goldmark's default) so user markdown can't inject arbitrary
// tags — the output is predictable and gets sanitized again below anyway.
var mdRenderer = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(gmhtml.WithHardWraps()),
)

// RenderMarkdown converts markdown source to a sanitized HTML fragment.
func RenderMarkdown(src string) string {
	var buf bytes.Buffer
	if err := mdRenderer.Convert([]byte(src), &buf); err != nil {
		// NOTE: goldmark.Convert basically never errors on []byte input;
		// fall back to escaped source rather than dropping the message body.
		return html.EscapeString(src)
	}
	return emailPolicy.Sanitize(buf.String())
}

// SanitizeComposeHTML cleans WYSIWYG-editor HTML down to the same allowlist used
// for incoming mail (formatting tags, <font>, safe inline styles, http/mailto
// links) so outgoing markup is predictable.
func SanitizeComposeHTML(fragment string) string {
	return emailPolicy.Sanitize(fragment)
}

// emailDocTemplate wraps an HTML fragment in a minimal, boring, email-safe
// document: a max-width container with a readable sans-serif base. Kept
// deliberately plain for broad client compatibility.
const emailDocHead = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body style="margin:0;padding:0;background:#ffffff;">
<div style="max-width:640px;margin:0 auto;padding:16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;font-size:15px;line-height:1.5;color:#1a1a1a;word-wrap:break-word;">
`
const emailDocTail = `
</div>
</body>
</html>`

// WrapHTMLEmail wraps a sanitized HTML body fragment in the email document.
func WrapHTMLEmail(fragment string) string {
	return emailDocHead + fragment + emailDocTail
}

// blockBreakTags close a visual block, so their end tag becomes a newline when
// flattening HTML to text.
var blockBreakTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true,
	"blockquote": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "ul": true, "ol": true,
}

// htmlToText produces a reasonable plain-text rendering of an HTML fragment for
// the text/plain alternative part: visible text with newlines at block
// boundaries. Not a full layout engine — just enough to read without markup.
func htmlToText(fragment string) string {
	z := xhtml.NewTokenizer(strings.NewReader(fragment))
	var b strings.Builder
	skip := 0
	for {
		tt := z.Next()
		if tt == xhtml.ErrorToken {
			break
		}
		switch tt {
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "head":
				skip++
			case "br":
				b.WriteByte('\n')
			}
		case xhtml.EndTagToken:
			n, _ := z.TagName()
			ns := string(n)
			if (ns == "script" || ns == "style" || ns == "head") && skip > 0 {
				skip--
			} else if blockBreakTags[ns] {
				b.WriteByte('\n')
			}
		case xhtml.TextToken:
			if skip == 0 {
				b.Write(z.Text())
			}
		}
	}
	// Collapse 3+ newlines to 2, trim trailing space per line.
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	out := strings.Join(lines, "\n")
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(out)
}
