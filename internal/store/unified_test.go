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
