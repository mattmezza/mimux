package mail

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"strings"
	"time"

	emmail "github.com/emersion/go-message/mail"

	"github.com/mattmezza/mimux/internal/config"
)

func htmlesc(s string) string { return html.EscapeString(s) }

// ComposeInput is everything needed to build one outgoing message: a new
// compose, reply, reply-all or forward all funnel through this. Addresses are
// raw RFC 5322 strings ("Name <addr>" or bare "addr"), as stored/typed.
type ComposeInput struct {
	To, Cc, Bcc []string
	Subject     string
	Body        string
	Mode        string // "plain" (default), "html" (Body is HTML) or "markdown" (Body is markdown)
	From        string // send-as address (account primary or an alias); "" = account default
	InReplyTo   string // original Message-ID (no angle brackets), "" for a new message
	References  string // full References header value to reuse (see ComputeReferences), "" for a new message
	Attachments []OutAttachment
}

// bodyParts derives the (plainText, htmlBody) pair to send for this input's
// mode. htmlBody == "" means send text/plain only (plain mode). For html and
// markdown modes both parts are produced and shipped as multipart/alternative.
func (in ComposeInput) bodyParts() (plain, htmlBody string) {
	switch in.Mode {
	case "html":
		safe := SanitizeComposeHTML(in.Body)
		return htmlToText(safe), WrapHTMLEmail(safe)
	case "markdown":
		return in.Body, WrapHTMLEmail(RenderMarkdown(in.Body))
	default:
		return in.Body, ""
	}
}

// OutAttachment is one file to attach to an outgoing message. Data is the raw
// (already decoded) file bytes; the writer base64-encodes it.
type OutAttachment struct {
	Filename    string
	ContentType string // "" defaults to application/octet-stream
	Data        []byte
}

// PrefixSubject adds "Re: "/"Fwd: " for kind "reply"/"reply_all"/"forward",
// unless the subject already carries that prefix (no stacking). Any other
// kind (e.g. "new") is returned unchanged.
func PrefixSubject(kind, subject string) string {
	var prefix string
	switch kind {
	case "reply", "reply_all":
		prefix = "Re: "
	case "forward":
		prefix = "Fwd: "
	default:
		return subject
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(subject)), strings.ToLower(prefix)) {
		return subject
	}
	return prefix + subject
}

// QuoteBody renders the classic reply quote block: a "On <date>, <from>
// wrote:" header followed by the original body with every line prefixed by
// "> ".
func QuoteBody(date time.Time, from, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "On %s, %s wrote:\n", date.Format("Mon, 2 Jan 2006 15:04"), from)
	for _, line := range strings.Split(body, "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// QuoteBodyHTML renders the reply quote for html (WYSIWYG) mode: an empty
// paragraph to type into, then the original wrapped in a <blockquote> with line
// breaks preserved. Body is plain text (the stored snippet), HTML-escaped.
func QuoteBodyHTML(date time.Time, from, body string) string {
	var b strings.Builder
	b.WriteString("<p><br></p><blockquote>")
	fmt.Fprintf(&b, "On %s, %s wrote:<br>", date.Format("Mon, 2 Jan 2006 15:04"), htmlesc(from))
	for i, line := range strings.Split(body, "\n") {
		if i > 0 {
			b.WriteString("<br>")
		}
		b.WriteString(htmlesc(line))
	}
	b.WriteString("</blockquote>")
	return b.String()
}

// SplitAddrList splits a comma-joined address list (as stored in
// store.Message.ToAddresses/CcAddresses, or typed into a compose field) into
// trimmed, non-empty entries.
func SplitAddrList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// bareAddr extracts the "addr" part of a "Name <addr>" string, or returns s
// unchanged if there's no angle-bracket form.
func bareAddr(a string) string {
	if i := strings.LastIndex(a, "<"); i >= 0 {
		return strings.TrimSuffix(a[i+1:], ">")
	}
	return strings.TrimSpace(a)
}

// dedupAddrs merges address lists, deduping by bare address (case
// insensitive) and preserving first-seen order/formatting.
func dedupAddrs(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, a := range l {
			a = strings.TrimSpace(a)
			key := strings.ToLower(bareAddr(a))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, a)
		}
	}
	return out
}

func excludeSelf(self string, addrs []string) []string {
	self = strings.ToLower(bareAddr(self))
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if strings.ToLower(bareAddr(a)) != self {
			out = append(out, a)
		}
	}
	return out
}

// ReplyRecipients computes the To list for a plain reply: the original
// sender, unless that happens to be ourselves.
func ReplyRecipients(self, from string) []string {
	return excludeSelf(self, []string{from})
}

// ReplyAllRecipients computes To/Cc for reply-all: To is the original sender
// plus original To (self and duplicates removed), Cc is the original Cc
// (self and anything already in To removed).
func ReplyAllRecipients(self string, from, to, cc []string) (toOut, ccOut []string) {
	toOut = excludeSelf(self, dedupAddrs(from, to))
	inTo := make(map[string]bool, len(toOut))
	for _, a := range toOut {
		inTo[strings.ToLower(bareAddr(a))] = true
	}
	for _, a := range excludeSelf(self, dedupAddrs(cc)) {
		if !inTo[strings.ToLower(bareAddr(a))] {
			ccOut = append(ccOut, a)
		}
	}
	return toOut, ccOut
}

// splitMsgIDs splits a References/In-Reply-To header value into bare
// (bracket-stripped) message ids.
func splitMsgIDs(s string) []string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if id := strings.Trim(f, "<> \t"); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// ComputeReferences returns the new References header value for a reply:
// the original message's own References with the original message's
// Message-ID appended (RFC 5322 section 3.6.4 threading), deduped.
func ComputeReferences(origRefs, origMessageID string) string {
	ids := splitMsgIDs(origRefs)
	if id := strings.Trim(strings.TrimSpace(origMessageID), "<> "); id != "" {
		found := false
		for _, e := range ids {
			if e == id {
				found = true
				break
			}
		}
		if !found {
			ids = append(ids, id)
		}
	}
	wrapped := make([]string, len(ids))
	for i, id := range ids {
		wrapped[i] = "<" + id + ">"
	}
	return strings.Join(wrapped, " ")
}

// toCRLF normalizes body text to CRLF line endings, as SMTP DATA requires.
func toCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func msgIDHost(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 && i+1 < len(email) {
		return email[i+1:]
	}
	return "mimux.local"
}

// parseAddrs turns raw address strings into mail.Address values for the
// header. A string that fails RFC 5322 address parsing is kept as a bare
// address rather than dropped, so a slightly malformed manual entry still
// gets sent instead of silently vanishing from the header.
func parseAddrs(raw []string) []*emmail.Address {
	out := make([]*emmail.Address, 0, len(raw))
	for _, r := range raw {
		if a, err := emmail.ParseAddress(r); err == nil {
			out = append(out, a)
			continue
		}
		out = append(out, &emmail.Address{Address: bareAddr(r)})
	}
	return out
}

// BuildMessage renders a ComposeInput into an RFC 5322 message for account
// cfg, returning the raw bytes and the generated Message-ID (without angle
// brackets). A plain-text single part when there are no attachments, otherwise
// a multipart/mixed with the text body plus one base64 attachment per file.
func BuildMessage(cfg config.Account, in ComposeInput, now time.Time) (raw []byte, messageID string, err error) {
	fromAddr := cfg.Email
	if in.From != "" {
		fromAddr = bareAddr(in.From)
	}
	var h emmail.Header
	h.SetAddressList("From", []*emmail.Address{{Name: cfg.DisplayNameFor(fromAddr), Address: fromAddr}})
	if to := parseAddrs(in.To); len(to) > 0 {
		h.SetAddressList("To", to)
	}
	if cc := parseAddrs(in.Cc); len(cc) > 0 {
		h.SetAddressList("Cc", cc)
	}
	// Bcc is intentionally never written to the header: the SMTP envelope
	// (RCPT TO) carries it instead, see Manager.Send.
	h.SetSubject(in.Subject)
	h.SetDate(now)
	if err := h.GenerateMessageIDWithHostname(msgIDHost(fromAddr)); err != nil {
		return nil, "", err
	}
	messageID, _ = h.MessageID()
	if in.InReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{strings.Trim(in.InReplyTo, "<> ")})
	}
	if in.References != "" {
		h.Set("References", in.References)
	}

	plain, htmlBody := in.bodyParts()

	var buf bytes.Buffer
	switch {
	case len(in.Attachments) > 0:
		if err := writeMultipart(&buf, h, in, plain, htmlBody); err != nil {
			return nil, "", err
		}
	case htmlBody != "":
		if err := writeAlternative(&buf, h, plain, htmlBody); err != nil {
			return nil, "", err
		}
	default:
		h.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
		w, err := emmail.CreateSingleInlineWriter(&buf, h)
		if err != nil {
			return nil, "", err
		}
		if _, err := io.WriteString(w, toCRLF(plain)); err != nil {
			return nil, "", err
		}
		if err := w.Close(); err != nil {
			return nil, "", err
		}
	}
	return buf.Bytes(), messageID, nil
}

// writeTextParts writes the text/plain part, and (when htmlBody != "") a
// text/html part, into an already-created multipart/alternative InlineWriter.
func writeTextParts(iw *emmail.InlineWriter, plain, htmlBody string) error {
	var tp emmail.InlineHeader
	tp.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	tw, err := iw.CreatePart(tp)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(tw, toCRLF(plain)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if htmlBody == "" {
		return nil
	}
	var hp emmail.InlineHeader
	hp.SetContentType("text/html", map[string]string{"charset": "utf-8"})
	hw, err := iw.CreatePart(hp)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(hw, toCRLF(htmlBody)); err != nil {
		return err
	}
	return hw.Close()
}

// writeAlternative emits a top-level multipart/alternative message with a
// text/plain and a text/html part (html/markdown compose, no attachments).
func writeAlternative(buf *bytes.Buffer, h emmail.Header, plain, htmlBody string) error {
	iw, err := emmail.CreateInlineWriter(buf, h)
	if err != nil {
		return err
	}
	if err := writeTextParts(iw, plain, htmlBody); err != nil {
		return err
	}
	return iw.Close()
}

// writeMultipart emits a multipart/mixed message: the body followed by one
// base64-encoded attachment per file. When htmlBody != "" the body is a nested
// multipart/alternative (text/plain + text/html); otherwise a single
// text/plain part. CreateWriter sets Content-Type to multipart/mixed.
func writeMultipart(buf *bytes.Buffer, h emmail.Header, in ComposeInput, plain, htmlBody string) error {
	mw, err := emmail.CreateWriter(buf, h)
	if err != nil {
		return err
	}
	if htmlBody != "" {
		iw, err := mw.CreateInline()
		if err != nil {
			return err
		}
		if err := writeTextParts(iw, plain, htmlBody); err != nil {
			return err
		}
		if err := iw.Close(); err != nil {
			return err
		}
	} else {
		var th emmail.InlineHeader
		th.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
		tw, err := mw.CreateSingleInline(th)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(tw, toCRLF(plain)); err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			return err
		}
	}
	for _, at := range in.Attachments {
		ct := at.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		var ah emmail.AttachmentHeader
		ah.SetContentType(ct, nil)
		ah.SetFilename(at.Filename)
		aw, err := mw.CreateAttachment(ah)
		if err != nil {
			return err
		}
		if _, err := aw.Write(at.Data); err != nil {
			return err
		}
		if err := aw.Close(); err != nil {
			return err
		}
	}
	return mw.Close()
}
