// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"regexp"
	"strings"
	"time"
)

// mailer is a func, not an interface: one real implementation, and tests swap
// it for a closure that appends to a slice.
type mailer func(to, subject, body string) error

// Three goes at a flaky link before giving up on a key somebody paid for.
const mailAttempts = 3

// Vars rather than consts only so the tests need not sit through them.
var (
	// One deadline for dial plus the whole conversation. A stalled submission
	// host used to hang the Stripe webhook until Stripe gave up and retried —
	// and the retry finds the event already claimed and sends nothing.
	smtpTimeout    = 30 * time.Second
	mailRetryDelay = 5 * time.Second
)

// dispatch sends in the background: every caller is on a request path where the
// far end (Stripe, or a person staring at /retrieve) must not wait on SMTP. ref
// is the licence id where there is one, for tying a failure back to a customer.
func (a *app) dispatch(ref, to, subject, body string) {
	a.mail.Add(1)
	go func() {
		defer a.mail.Done()
		for attempt := 1; ; attempt++ {
			err := a.send(to, subject, body)
			if err == nil {
				return
			}
			slog.Error("email failed", "ref", ref, "subject", subject, "attempt", attempt, "err", err)
			if attempt == mailAttempts {
				slog.Error("giving up on email — customer must use /retrieve", "ref", ref)
				return
			}
			time.Sleep(time.Duration(attempt) * mailRetryDelay)
		}
	}()
}

// smtpSender speaks the submission conversation by hand rather than calling
// smtp.SendMail, which cannot be given a timeout. STARTTLS is negotiated when
// the server offers it, as SendMail did. ponytail: no implicit-TLS (port 465)
// support — use the submission port 587, every provider speaks it. Add a
// tls.Dial branch if that ever changes.
func smtpSender(c config) mailer {
	return func(to, subject, body string) error {
		msg, err := buildMessage(c.smtpFrom, to, subject, body)
		if err != nil {
			return err
		}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(c.smtpHost, c.smtpPort), smtpTimeout)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		// Absolute: a server that accepts the connection and then goes quiet is
		// the failure that has no other bound.
		if err := conn.SetDeadline(time.Now().Add(smtpTimeout)); err != nil {
			return err
		}
		cl, err := smtp.NewClient(conn, c.smtpHost)
		if err != nil {
			return err
		}
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(&tls.Config{ServerName: c.smtpHost}); err != nil {
				return err
			}
		}
		if c.smtpUser != "" {
			if err := cl.Auth(smtp.PlainAuth("", c.smtpUser, c.smtpPass, c.smtpHost)); err != nil {
				return err
			}
		}
		if err := cl.Mail(c.smtpFrom); err != nil {
			return err
		}
		if err := cl.Rcpt(to); err != nil {
			return err
		}
		w, err := cl.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write(msg); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		return cl.Quit()
	}
}

// buildMessage assembles a multipart/alternative message: the plain text body
// exactly as written, and the same words rendered in the house style for
// clients that prefer HTML. Text goes first — multipart/alternative is
// least-preferred-first, and the text part is what has to survive a client that
// renders nothing else.
//
// Recipient, sender and subject are rejected outright if they contain CR or LF
// — that is header injection, and the recipient address comes from user input
// on /retrieve.
//
// Both parts are quoted-printable. SMTP allows ~998 bytes per line and a
// licence key on one line, or any line of the HTML, can pass that; quoted
// -printable soft-wraps and the decoder puts it back byte for byte, which
// nothing about wrapping the source by hand would guarantee.
func buildMessage(from, to, subject, body string) ([]byte, error) {
	if !validEmail(to) {
		return nil, fmt.Errorf("invalid recipient address")
	}
	if strings.ContainsAny(subject, "\r\n") {
		return nil, fmt.Errorf("invalid subject")
	}
	if strings.ContainsAny(from, "\r\n") {
		return nil, fmt.Errorf("invalid sender")
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")

	var parts bytes.Buffer
	mp := multipart.NewWriter(&parts)
	// quotedprintable.Writer turns \n into CRLF itself, so the bodies stay in
	// Go's line endings right up to the wire.
	write := func(contentType, content string) error {
		w, err := mp.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {contentType},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return err
		}
		q := quotedprintable.NewWriter(w)
		if _, err := io.WriteString(q, content); err != nil {
			return err
		}
		return q.Close()
	}
	if err := write("text/plain; charset=utf-8", body); err != nil {
		return nil, err
	}
	if err := write("text/html; charset=utf-8", htmlBody(subject, body)); err != nil {
		return nil, err
	}
	if err := mp.Close(); err != nil {
		return nil, err
	}

	h := []string{
		"From: mimux <" + from + ">",
		"To: " + to,
		"Subject: " + subject,
		"Date: " + time.Now().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		`Content-Type: multipart/alternative; boundary="` + mp.Boundary() + `"`,
	}
	return append([]byte(strings.Join(h, "\r\n")+"\r\n\r\n"), parts.Bytes()...), nil
}

func validEmail(s string) bool {
	if s == "" || len(s) > 254 || strings.ContainsAny(s, "\r\n") {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}

// The correspondence palette from templates/layout.html, converted out of
// oklch(): mail clients do not speak it, and half of them do not speak <style>
// blocks either, so every rule here is inlined on the element it styles. Keep
// these in step with the web pages when the brand moves.
const (
	mailPaper  = "#f7f3eb" // --paper-50
	mailCard   = "#fdfbf7" // --card
	mailInk    = "#2a1f19" // --ink-950
	mailInk800 = "#3e332c" // --ink-800
	mailInk500 = "#817873" // --ink-500
	mailAccent = "#be2612" // --accent-600
	mailStripe = "#d33e25" // --accent-500
	mailPost   = "#3a5c9b" // --post-500
	mailBorder = "#d7d3cf" // ink-950 at 18% over card
	mailRule   = "#d8d3cc" // ink-950 at 15% over paper

	// The house fonts are woff2 files on account.mimux.dev; no mail client will
	// fetch them. These are the same fallback chains layout.html already names.
	mailSans = "-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif"
	mailMono = "ui-monospace,SFMono-Regular,Menlo,Consolas,'Liberation Mono',monospace"
)

// htmlBody renders one of the plain-text bodies as HTML. There is one renderer
// rather than a hand-written HTML twin of each message, so the two parts cannot
// drift: the text is the source and the shape it already has carries over.
// Blocks are separated by blank lines; a block indented four spaces is a key or
// a link and gets the monospace box; anything else is a paragraph, reflowed
// (the text is hard-wrapped for a terminal, HTML wraps itself).
func htmlBody(subject, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="format-detection" content="telephone=no">
<title>%s</title></head>
<body style="margin:0;padding:0;background:%s;">
<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" border="0" style="background:%s;">
<tr><td align="center" style="padding:24px 12px;">
<table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0" style="width:600px;max-width:100%%;background:%s;border:1px solid %s;border-radius:6px;">
<tr><td style="padding:0;font-size:0;line-height:0;">%s</td></tr>
<tr><td style="padding:26px 32px 0;">
<div style="font-family:%s;font-size:20px;font-weight:700;letter-spacing:-0.02em;color:%s;">mimux <span style="color:%s;">pro</span></div>
<div style="font-family:%s;font-size:11px;font-weight:500;letter-spacing:0.16em;text-transform:uppercase;color:%s;padding-top:10px;">%s</div>
</td></tr>
<tr><td style="padding:18px 32px 4px;font-family:%s;font-size:16px;line-height:1.65;color:%s;">
`,
		template.HTMLEscapeString(subject),
		mailPaper, mailPaper, mailCard, mailBorder,
		airmailBand(),
		mailSans, mailInk, mailAccent,
		mailMono, mailAccent, template.HTMLEscapeString(subject),
		mailSans, mailInk800)

	for _, block := range strings.Split(strings.Trim(body, "\n"), "\n\n") {
		lines := strings.Split(strings.Trim(block, "\n"), "\n")
		if text, ok := dedent(lines); ok {
			b.WriteString(keyBox(text))
			continue
		}
		var joined []string
		for _, l := range lines {
			if l = strings.TrimSpace(l); l != "" {
				joined = append(joined, l)
			}
		}
		if len(joined) == 0 {
			continue
		}
		fmt.Fprintf(&b, "<p style=\"margin:0 0 16px;\">%s</p>\n", inlineHTML(strings.Join(joined, " ")))
	}

	// The footer says who sent this and nothing else: the bodies already carry
	// their own closing lines, and repeating them here would only make the mail
	// longer than the thing it is delivering.
	fmt.Fprintf(&b, `</td></tr>
<tr><td style="padding:8px 32px 26px;">
<div style="border-top:1px solid %s;padding-top:14px;font-family:%s;font-size:12px;letter-spacing:0.08em;text-transform:uppercase;color:%s;">
mimux pro &middot; account.mimux.dev
</div>
</td></tr>
</table>
</td></tr></table>
</body></html>
`, mailRule, mailMono, mailInk500)
	return b.String()
}

// airmailBand is the masthead stripe of the whole brand, drawn as a row of
// coloured table cells rather than the site's repeating gradient: no image (a
// remote image in mail is a tracking pixel wearing a hat, and this product's
// whole claim is that nothing phones home) and no CSS gradient (Outlook).
func airmailBand() string {
	var b strings.Builder
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>`)
	// 20 cells of 5%: integer widths that add up to exactly 100, so the band
	// reaches both edges in clients that do not do fractional percentages.
	for i := range 20 {
		colour := mailPaper
		switch i % 4 {
		case 0:
			colour = mailStripe
		case 2:
			colour = mailPost
		}
		fmt.Fprintf(&b, `<td width="5%%" height="8" style="background:%s;font-size:0;line-height:8px;">&nbsp;</td>`, colour)
	}
	b.WriteString(`</tr></table>`)
	return b.String()
}

// keyBox holds the licence key. It is the one thing in the message that must
// arrive character-perfect: <pre> so nothing reflows it, word-break so a long
// key wraps visibly instead of stretching the mail off the screen, and no
// linkification of anything that is not already a URL — a client that turns a
// key into a link hands the customer something that will not verify.
func keyBox(text string) string {
	inner := template.HTMLEscapeString(text)
	if u := strings.TrimSpace(text); strings.HasPrefix(u, "https://") && !strings.ContainsAny(u, " \n") {
		inner = fmt.Sprintf(`<a href="%s" style="color:%s;">%s</a>`,
			template.HTMLEscapeString(u), mailAccent, template.HTMLEscapeString(u))
	}
	return fmt.Sprintf(`<div style="margin:0 0 18px;background:%s;border:1px solid %s;border-left:3px solid %s;border-radius:0 4px 4px 0;padding:12px 14px;">`+
		`<pre style="margin:0;font-family:%s;font-size:13px;line-height:1.5;color:%s;white-space:pre-wrap;word-break:break-all;overflow-wrap:break-word;">%s</pre></div>`+"\n",
		mailPaper, mailBorder, mailStripe, mailMono, mailInk, inner)
}

// dedent reports whether every line of a block is indented — the plain-text
// bodies indent by four spaces exactly where a key or a link stands alone.
func dedent(lines []string) (string, bool) {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !strings.HasPrefix(l, "    ") {
			return "", false
		}
		out = append(out, strings.TrimSpace(l))
	}
	if len(out) == 0 {
		return "", false
	}
	return strings.Join(out, "\n"), true
}

// linkRe finds the URLs and addresses the plain text writes out in full, so the
// HTML can make them clickable. Deliberately narrow: it never matches a licence
// key, which has no scheme and no @.
var linkRe = regexp.MustCompile(`https?://[^\s<>"]+|[\w.+-]+@[\w-]+\.[\w.-]*[\w]`)

// inlineHTML escapes a paragraph and links what deserves it.
func inlineHTML(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range linkRe.FindAllStringIndex(s, -1) {
		start, end := m[0], m[1]
		for end > start && strings.ContainsRune(".,;:)", rune(s[end-1])) {
			end--
		}
		text := s[start:end]
		href := text
		if !strings.Contains(text, "://") {
			href = "mailto:" + text
		}
		b.WriteString(template.HTMLEscapeString(s[last:start]))
		fmt.Fprintf(&b, `<a href="%s" style="color:%s;">%s</a>`,
			template.HTMLEscapeString(href), mailAccent, template.HTMLEscapeString(text))
		last = end
	}
	b.WriteString(template.HTMLEscapeString(s[last:]))
	return b.String()
}
