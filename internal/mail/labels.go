package mail

import "strings"

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
