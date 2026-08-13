package mail

import (
	"sort"
	"strings"
)

// gmailSystemLabels are Gmail's built-in labels (returned as \Name); they are
// already represented elsewhere in the UI (folders, star, unread) so they are
// hidden from the label pills.
var gmailSystemLabels = map[string]bool{
	"inbox": true, "sent": true, "draft": true, "drafts": true, "spam": true,
	"trash": true, "important": true, "starred": true, "unread": true, "all mail": true,
}

// MessageLabels parses a stored space-joined X-GM-LABELS value into a display
// list of user labels: quotes stripped, Gmail system labels (\Inbox, …)
// dropped. Order is preserved and duplicates removed.
func MessageLabels(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Fields(raw) {
		l := strings.Trim(f, `"`)
		if l == "" || strings.HasPrefix(l, `\`) {
			continue // system label like \Inbox
		}
		if l = strings.ReplaceAll(l, "_", " "); gmailSystemLabels[strings.ToLower(l)] {
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
// the space-joined labels column: trimmed, quotes and a leading backslash
// dropped, inner whitespace joined with "_" — the convention MessageLabels
// already reverses on display, and the reason the column can hold labels with
// spaces at all. "" when nothing usable is left or the name is a Gmail system
// label (those are rendered as folders/star/unread, never as pills).
func LabelToken(s string) string {
	s = strings.Join(strings.Fields(strings.Trim(strings.TrimSpace(s), `"\`)), "_")
	if s == "" || gmailSystemLabels[strings.ToLower(strings.ReplaceAll(s, "_", " "))] {
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
		if !strings.EqualFold(strings.Trim(f, `"`), l) {
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

func hasLabel(raw, token string) bool {
	for _, f := range strings.Fields(raw) {
		if strings.EqualFold(strings.Trim(f, `"`), token) {
			return true
		}
	}
	return false
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
