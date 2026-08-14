package mail

import (
	"sort"
	"strings"
)

// noiseLabels is the denylist of flags/labels that are hidden from the label
// pills: standard IMAP system flags, Gmail's internal $-keywords, and Gmail
// system labels that already have first-class UI elsewhere (folders, star,
// unread). Matched case-insensitively after normalizeLabel strips the \ or $
// sigil and turns "_" back into a space, so \Seen and $Junk match by bare
// name. Everything else — including bare keywords like Triaged, or the same
// arriving as \Triaged or $Triaged — is shown.
var noiseLabels = map[string]bool{
	// standard IMAP system flags (RFC 3501)
	"seen": true, "answered": true, "flagged": true, "deleted": true,
	"draft": true, "recent": true,
	// Gmail's internal $ keywords
	"junk": true, "notjunk": true, "phishing": true,
	// Gmail system labels with first-class UI elsewhere
	"inbox": true, "sent": true, "drafts": true, "spam": true, "trash": true,
	"important": true, "starred": true, "unread": true, "all mail": true, "chat": true,
}

// normalizeLabel strips a leading IMAP flag sigil (\ or $) and enclosing
// quotes, and turns the stored "_" convention back into the spaces used for
// display and for matching against noiseLabels.
func normalizeLabel(s string) string {
	return strings.ReplaceAll(strings.TrimLeft(strings.Trim(s, `"`), `\$`), "_", " ")
}

// isNoiseLabel reports whether a normalized label name is one of the
// genuinely-internal flags or system labels that already have first-class UI
// — the denylist MessageLabels and LabelToken both hide behind. Gmail's
// Category_* tabs (Promotions, Updates, …) are matched by prefix since Gmail
// sends them as one token, e.g. \CategoryPromotions.
func isNoiseLabel(name string) bool {
	n := strings.ToLower(name)
	return noiseLabels[n] || strings.HasPrefix(n, "category")
}

// MessageLabels parses a stored space-joined label value (Gmail's
// X-GM-LABELS plus any user-added tokens) into a display list: quotes and
// sigil stripped, noise (isNoiseLabel) dropped. Order is preserved and
// duplicates removed.
func MessageLabels(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Fields(raw) {
		l := normalizeLabel(f)
		if l == "" || isNoiseLabel(l) {
			continue
		}
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

// LabelToken turns a user-typed (or displayed) label into the token stored in
// the space-joined labels column: trimmed, quotes and a leading sigil
// dropped, inner whitespace joined with "_" — the convention MessageLabels
// already reverses on display, and the reason the column can hold labels with
// spaces at all. "" when nothing usable is left or the name is noise (see
// isNoiseLabel — those are rendered as folders/star/unread, never as pills).
func LabelToken(s string) string {
	s = strings.Join(strings.Fields(strings.Trim(strings.TrimSpace(s), `"\$`)), "_")
	if s == "" || isNoiseLabel(strings.ReplaceAll(s, "_", " ")) {
		return ""
	}
	return s
}

// AddLabel appends a user label to a stored space-joined label value, leaving
// every existing token (including Gmail's \System ones) untouched. Returns raw
// unchanged when the label is unusable or already there.
func AddLabel(raw, label string) string {
	l := LabelToken(label)
	if l == "" || hasLabel(raw, l) {
		return raw
	}
	if raw == "" {
		return l
	}
	return raw + " " + l
}

// RemoveLabel drops a user label from a stored space-joined value, matching on
// the normalized form so the displayed "Project X" removes a stored Project_X.
func RemoveLabel(raw, label string) string {
	l := LabelToken(label)
	if l == "" {
		return raw
	}
	fields := strings.Fields(raw)
	out := fields[:0]
	for _, f := range fields {
		if !strings.EqualFold(strings.TrimLeft(strings.Trim(f, `"`), `\$`), l) {
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

func hasLabel(raw, token string) bool {
	for _, f := range strings.Fields(raw) {
		if strings.EqualFold(strings.TrimLeft(strings.Trim(f, `"`), `\$`), token) {
			return true
		}
	}
	return false
}

// MergeLabels folds newly-fetched IMAP flags into a stored label value: every
// non-noise flag (isNoiseLabel) not already present is appended verbatim —
// sigil kept as-is, for round-tripping to IMAP — and matched against what's
// already there sigil-insensitively (hasLabel), so a bare "Triaged" from a
// previous sync and a later "\Triaged" from the server dedup as the same
// label. Existing tokens are never dropped, including labels applied locally
// that the server has no way to report back (see Manager.SetLabel): a flag
// no longer set on the server merely goes stale here until removed by hand,
// same trade AddLabel already makes for user-added labels.
func MergeLabels(existing string, flags []string) string {
	for _, f := range flags {
		n := normalizeLabel(f)
		if n == "" || isNoiseLabel(n) || hasLabel(existing, n) {
			continue
		}
		if existing == "" {
			existing = f
		} else {
			existing += " " + f
		}
	}
	return existing
}

// AllLabels flattens stored label values into the sorted, deduped set of
// display labels in use — the autocomplete source for the add-label popover.
func AllLabels(raws []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range raws {
		for _, l := range MessageLabels(raw) {
			if !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	sort.Strings(out)
	return out
}
