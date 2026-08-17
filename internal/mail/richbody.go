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
// deliberately plain for broad client compatibility. The container div keeps
// its inline base styles (font/size/line-height/color) so clients that strip
// <head><style> still get readable text; the scoped .mimux-body stylesheet layers
// on the per-element polish (headings, links, code, blockquotes, tables) for
// the clients that keep it. Everything is scoped under .mimux-body so it can't
// bleed into quoted replies or the surrounding message.
const emailDocHead = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<style>
.mimux-body h1,.mimux-body h2,.mimux-body h3,.mimux-body h4{line-height:1.25;margin:1.4em 0 .5em;font-weight:600}
.mimux-body h1{font-size:1.6em}.mimux-body h2{font-size:1.35em}.mimux-body h3{font-size:1.15em}
.mimux-body p,.mimux-body ul,.mimux-body ol,.mimux-body blockquote,.mimux-body pre,.mimux-body table{margin:0 0 1em}
.mimux-body li{margin:.25em 0}
.mimux-body a{color:#2563eb;text-decoration:underline}
.mimux-body img{max-width:100%;height:auto}
.mimux-body hr{border:0;border-top:1px solid #e5e5e5;margin:1.5em 0}
.mimux-body blockquote{margin-left:0;padding:.2em 1em;border-left:3px solid #d0d0d0;color:#555}
.mimux-body code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:.9em;background:#f2f2f2;padding:.15em .35em;border-radius:4px}
.mimux-body pre{background:#f6f8fa;padding:12px 14px;border-radius:6px;overflow-x:auto;line-height:1.45}
.mimux-body pre code{background:none;padding:0;font-size:.88em}
.mimux-body table{border-collapse:collapse;width:100%}
.mimux-body th,.mimux-body td{border:1px solid #e0e0e0;padding:6px 10px;text-align:left}
.mimux-body th{background:#f6f8fa;font-weight:600}
</style></head>
<body style="margin:0;padding:0;background:#ffffff;">
<div class="mimux-body" style="max-width:640px;margin:0 auto;padding:16px;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;font-size:15px;line-height:1.5;color:#1a1a1a;word-wrap:break-word;">
`
const emailDocTail = `
</div>
</body>
</html>`

// WrapHTMLEmail wraps a sanitized HTML body fragment in the email document.
func WrapHTMLEmail(fragment string) string {
	return emailDocHead + fragment + emailDocTail
}

// ConvertBody rewrites a compose body when the user switches the per-message
// format (plain / markdown / html). from and to are the compose modes; an
// unknown mode behaves like "plain", matching bodyParts.
//
// NOTE: html -> markdown is just html -> text — there is no HTML-to-markdown
// translator in here and one file of regexes would be worse than the flattening
// htmlToText already does well. Text survives, markup doesn't. plain <-> markdown
// is a no-op because markdown source already reads as plain text (it is exactly
// what bodyParts ships as the text/plain part). Add a real converter only if
// round-tripping formatting turns out to matter.
func ConvertBody(body, from, to string) string {
	if from == to || strings.TrimSpace(body) == "" {
		return body
	}
	switch {
	case from == "html":
		return htmlToText(SanitizeComposeHTML(body))
	case to == "html":
		if from == "markdown" {
			return RenderMarkdown(body)
		}
		return textToHTML(body)
	}
	return body
}

// textToHTML paragraph-izes plain text for the WYSIWYG editor: a blank line
// starts a new <p>, a single newline becomes a <br>.
func textToHTML(s string) string {
	var b strings.Builder
	for _, para := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n") {
		b.WriteString("<p>")
		b.WriteString(strings.ReplaceAll(html.EscapeString(strings.TrimSpace(para)), "\n", "<br>"))
		b.WriteString("</p>")
	}
	return b.String()
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
