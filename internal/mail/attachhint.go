// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"regexp"
	"strings"
)

// attachWords are the (English + Italian) stems that suggest the writer meant
// to attach a file. Matched as whole words, case-insensitively. Kept in sync
// with the same list mirrored in web/static/js/app.js (attachKeywords).
var attachWords = []string{
	"attach", "attached", "attachment", "attachments", "attaching",
	"enclosed",
	"allegato", "allegati", "allegata", "allegate", "allego",
}

// attachRe matches any attachWords entry as a whole word (word boundaries so
// "attach" won't fire inside "attaches"—actually it will via the stem; the
// boundary just avoids matching inside unrelated longer tokens like "detach").
var attachRe = func() *regexp.Regexp {
	quoted := make([]string, len(attachWords))
	for i, w := range attachWords {
		quoted[i] = regexp.QuoteMeta(w)
	}
	return regexp.MustCompile(`(?i)\b(` + strings.Join(quoted, "|") + `)\b`)
}()

var (
	// Tokenize tags without mistaking > inside a quoted attribute, comments,
	// or similarly named elements for blockquote boundaries.
	htmlQuoteTokenRe     = regexp.MustCompile(`(?is)<!--.*?-->|</?([a-z][a-z0-9-]*)\b(?:[^>"']|"[^"]*"|'[^']*')*>`)
	replyAttributionRe   = regexp.MustCompile(`(?i)^[ \t]*On .+ wrote:[ \t]*\r?$`)
	forwardAttributionRe = regexp.MustCompile(`(?i)^[ \t]*---------- Forwarded message ----------[ \t]*\r?$`)
	quotedLineRe         = regexp.MustCompile(`^[ \t]*>`)
)

// MentionsAttachment reports whether text authored in a compose hints at an
// attachment ("please find attached", "in allegato", …). Only explicit quote
// regions are excluded; unquoted inline and bottom replies still count.
func MentionsAttachment(text string) bool {
	return attachRe.MatchString(stripQuotedText(text))
}

// stripHTMLQuotes removes complete, potentially nested blockquote regions.
// An unmatched opening tag is kept: ambiguous markup must not hide authored text.
func stripHTMLQuotes(text string) string {
	var out strings.Builder
	depth, start, kept := 0, 0, 0
	for _, token := range htmlQuoteTokenRe.FindAllStringSubmatchIndex(text, -1) {
		if token[2] < 0 || !strings.EqualFold(text[token[2]:token[3]], "blockquote") {
			continue
		}
		if strings.HasPrefix(text[token[0]:token[1]], "</") {
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 {
				out.WriteString(text[kept:start])
				out.WriteByte('\n')
				kept = token[1]
			}
		} else {
			if depth == 0 {
				start = token[0]
			}
			depth++
		}
	}
	out.WriteString(text[kept:])
	return out.String()
}

// stripQuotedText removes explicit quote lines and only the attribution that
// introduces them. Legacy unprefixed forwarded text has no reliable boundary;
// retain it rather than silently discarding an inline or bottom-posted reply.
func stripQuotedText(text string) string {
	lines := strings.Split(stripHTMLQuotes(text), "\n")
	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		if quotedLineRe.MatchString(line) {
			continue
		}
		if replyAttributionRe.MatchString(line) || forwardAttributionRe.MatchString(line) {
			next := i + 1
			for next < len(lines) && strings.TrimSpace(lines[next]) == "" {
				next++
			}
			if next < len(lines) && quotedLineRe.MatchString(lines[next]) {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
