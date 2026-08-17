// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestDetectSpecialUseFromAttrs(t *testing.T) {
	cases := []struct {
		attr imap.MailboxAttr
		want string
	}{
		{imap.MailboxAttrSent, "sent"},
		{imap.MailboxAttrDrafts, "drafts"},
		{imap.MailboxAttrTrash, "trash"},
		{imap.MailboxAttrJunk, "spam"},
		{imap.MailboxAttrArchive, "archive"},
	}
	for _, c := range cases {
		if got := detectSpecialUse([]imap.MailboxAttr{c.attr}, "Whatever"); got != c.want {
			t.Errorf("attr %s => %q, want %q", c.attr, got, c.want)
		}
	}
}

func TestDetectSpecialUseHeuristic(t *testing.T) {
	cases := map[string]string{
		"INBOX":             "inbox",
		"Sent":              "sent",
		"Sent Items":        "sent",
		"[Gmail]/Sent Mail": "sent",
		"Drafts":            "drafts",
		"Junk":              "spam",
		"Spam":              "spam",
		"Trash":             "trash",
		"Deleted Items":     "trash",
		"Deleted Messages":  "trash",
		"Archive":           "archive",
		"All Mail":          "archive",
		"Work/Projects":     "",
		"Newsletters":       "",
	}
	for name, want := range cases {
		if got := detectSpecialUse(nil, name); got != want {
			t.Errorf("name %q => %q, want %q", name, got, want)
		}
	}
}

func TestDetectSpecialUseAttrsBeatNameHeuristic(t *testing.T) {
	// A folder literally named "Trash" but flagged \Sent should follow the flag.
	if got := detectSpecialUse([]imap.MailboxAttr{imap.MailboxAttrSent}, "Trash"); got != "sent" {
		t.Errorf("attrs should win: got %q", got)
	}
}
