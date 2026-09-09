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
		{"\n\nOn Mon, 20 Jul 2026 10:30, Alice <alice@example.com> wrote:\n> Please find the invoice attached.\n", false},
		{"My attachment is in this reply.\n\nOn Mon, 20 Jul 2026 10:30, Alice <alice@example.com> wrote:\n> Please find the invoice attached.\n", true},
		{"\n\n---------- Forwarded message ----------\nFrom: Alice <alice@example.com>\n\nPlease find the invoice attached.", false},
		{"<p><br></p><blockquote>Please find the invoice attached.</blockquote>", false},
		{"<p>I attached the invoice.</p><blockquote>Please find the invoice attached.</blockquote>", true},
		{"On Monday, I wrote: attached files are ready", true},
		{"On Monday, I wrote:\nwithout quoting anyone", false},
		{"My reply\n> Please find the invoice attached.\nStill no file mention", false},
	}
	for _, c := range cases {
		if got := MentionsAttachment(c.text); got != c.want {
			t.Errorf("MentionsAttachment(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}
