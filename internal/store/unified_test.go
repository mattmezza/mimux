package store

import (
	"fmt"
	"testing"
	"time"
)

func TestListUnifiedInboxOrderingAcrossAccounts(t *testing.T) {
	s := open(t)

	// Two accounts, each with an inbox and a non-inbox folder.
	inboxA, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	inboxB, _ := s.UpsertFolder("B", "INBOX", "inbox", 0)
	sentA, _ := s.UpsertFolder("A", "Sent", "sent", 1)

	mk := func(account string, folder int64, uid uint32, min int) {
		if err := s.UpsertMessage(&Message{
			Account: account, FolderID: folder, UID: uid,
			MessageID: fmt.Sprintf("m%d@x", uid),
			Date:      time.Date(2026, 1, 1, 0, min, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("A", inboxA, 1, 10) // oldest
	mk("B", inboxB, 2, 30) // newest
	mk("A", inboxA, 3, 20) // middle
	mk("A", sentA, 4, 40)  // newest overall but NOT an inbox → excluded

	msgs, err := s.ListUnifiedInbox(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("want 3 inbox messages (sent excluded), got %d", len(msgs))
	}
	// Newest first, merged across accounts.
	wantOrder := []uint32{2, 3, 1}
	for i, uid := range wantOrder {
		if msgs[i].UID != uid {
			t.Errorf("position %d = uid %d, want %d", i, msgs[i].UID, uid)
		}
	}
}

// An unread message older than the newest-`limit` window must still be listed:
// otherwise the Unread filter shows fewer threads than the tab badge counts,
// and BuildThreads never sees the older sibling of a thread.
func TestListUnreadOutsideWindow(t *testing.T) {
	s := open(t)
	inbox, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	mk := func(uid uint32, day int, read bool) {
		if err := s.UpsertMessage(&Message{
			Account: "A", FolderID: inbox, UID: uid,
			MessageID: fmt.Sprintf("m%d@x", uid), Subject: "same subject",
			Date:   time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC),
			IsRead: read,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk(1, 1, false) // oldest, unread → outside a limit=2 window
	mk(2, 2, true)
	mk(3, 3, true)
	mk(4, 4, false) // newest, unread

	for _, tc := range []struct {
		name string
		got  func() ([]Message, error)
	}{
		{"folder", func() ([]Message, error) { return s.ListMessages(inbox, 2) }},
		{"unified", func() ([]Message, error) { return s.ListUnifiedInbox(2) }},
	} {
		msgs, err := tc.got()
		if err != nil {
			t.Fatal(err)
		}
		// window {4,3} plus the out-of-window unread {1}.
		want := []uint32{4, 3, 1}
		if len(msgs) != len(want) {
			t.Fatalf("%s: got %d messages, want %d", tc.name, len(msgs), len(want))
		}
		for i, uid := range want {
			if msgs[i].UID != uid {
				t.Errorf("%s: position %d = uid %d, want %d", tc.name, i, msgs[i].UID, uid)
			}
		}
	}
}
