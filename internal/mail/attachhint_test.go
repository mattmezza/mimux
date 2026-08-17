// SPDX-License-Identifier: AGPL-3.0-only
package mail

import "testing"

func TestMentionsAttachment(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"Please find the file attached.", true},
		{"See attachment below", true},
		{"I've added two attachments", true},
		{"Attaching the report now", true},
		{"Enclosed is the invoice", true},
		{"Ti mando il file in allegato", true},
		{"Trovi gli allegati qui sotto", true},
		{"Allego la fattura", true},
		{"ATTACHED you'll find it", true},   // case-insensitive
		{"Ecco l'allegata relazione", true}, // Italian feminine
		{"", false},
		{"Just a quick note, no files here", false},
		{"Let's detach the trailer", false}, // must not match inside "detach"
		{"Check https://x.com/attachment.pdf", true},
	}
	for _, c := range cases {
		if got := MentionsAttachment(c.text); got != c.want {
			t.Errorf("MentionsAttachment(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}
