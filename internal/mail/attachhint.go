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

// MentionsAttachment reports whether text hints at an attachment ("please find
// attached", "in allegato", …). Used to warn before sending with no files.
// Quote/reply markers are not stripped — a false positive on a quoted "attached"
// is cheaper than a missed reminder.
func MentionsAttachment(text string) bool {
	return attachRe.MatchString(text)
}
