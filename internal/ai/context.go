// SPDX-License-Identifier: AGPL-3.0-only
package ai

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Msg is one message of a conversation as the reply features see it: who wrote
// it, when, about what, and its readable text. No addresses beyond the From
// line, no markup, no attachments.
type Msg struct {
	From    string
	Date    time.Time
	Subject string
	Text    string
}

// MaxContextChars caps the whole conversation handed to the model. Long enough
// for a real thread, short enough to keep one suggestion cheap — and the chat
// endpoint takes whatever it is given, so the cap has to live here.
// NOTE: characters, not tokens — no tokenizer dependency for a rough budget.
// Same order as maxSummaryChars, which has held up for the summary feature.
const MaxContextChars = 12000

// maxThreadMsgChars caps one earlier message, so a single long mail in the
// middle of a thread cannot crowd every other turn out of the budget.
const maxThreadMsgChars = 2000

// contextFloor is the point at which the remaining budget is too small to be
// worth another message: below it a block would be mostly header.
const contextFloor = 400

// BuildContext renders the conversation the reply features work from: the
// message being replied to, preceded by as much of the earlier thread as the
// budget allows. thread must not contain orig; its order does not matter, it is
// sorted here.
//
// Truncation rule: the message being replied to is rendered first and keeps
// whatever it needs of MaxContextChars — it is the one the reply answers.
// Earlier messages are then taken newest-first until the budget runs out, and
// printed oldest-first so the model reads the exchange in order. Anything cut
// short ends with a marker, so the model knows it is seeing part of a message
// rather than a message that stops mid-sentence.
func BuildContext(orig Msg, thread []Msg) string {
	budget := MaxContextChars
	last := renderMsg("The message I am replying to", orig, &budget, MaxContextChars)

	sorted := slices.Clone(thread)
	slices.SortFunc(sorted, func(a, b Msg) int { return a.Date.Compare(b.Date) })
	var earlier []string
	for i := len(sorted) - 1; i >= 0 && budget > contextFloor; i-- {
		if strings.TrimSpace(sorted[i].Text) == "" {
			continue
		}
		earlier = append(earlier, renderMsg("Earlier in this conversation", sorted[i], &budget, min(maxThreadMsgChars, budget)))
	}
	slices.Reverse(earlier)
	return strings.Join(append(earlier, last), "\n")
}

// renderMsg writes one message as a labelled block and charges it to the
// budget, keeping at most limit characters of its text.
func renderMsg(label string, m Msg, budget *int, limit int) string {
	text := strings.TrimSpace(m.Text)
	if r := []rune(text); len(r) > limit {
		text = strings.TrimSpace(string(r[:limit])) + " " + truncMarker
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s ---\nFrom: %s\n", label, m.From)
	if !m.Date.IsZero() {
		fmt.Fprintf(&b, "Date: %s\n", m.Date.Format("Mon, 2 Jan 2006 15:04"))
	}
	if m.Subject != "" {
		fmt.Fprintf(&b, "Subject: %s\n", m.Subject)
	}
	fmt.Fprintf(&b, "\n%s\n", text)
	*budget -= b.Len()
	return b.String()
}

// truncMarker closes a message the budget cut short.
const truncMarker = "[…]"
