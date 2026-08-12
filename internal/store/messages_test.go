package store

import (
	"testing"
	"time"
)

// TestUpsertMessageRefreshesDate: a re-fetch of an existing UID must overwrite a
// stale date (regression for now()-stamped rows written before the INTERNALDATE
// fallback), while leaving read/star intact per the flag columns.
func TestUpsertMessageRefreshesDate(t *testing.T) {
	s := open(t)
	f, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)

	stale := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC) // wrong now()-stamp
	real := time.Date(2026, 7, 1, 13, 10, 0, 0, time.UTC) // true received time
	m := &Message{Account: "A", FolderID: f, UID: 1, Subject: "hi", Date: stale}
	if err := s.UpsertMessage(m); err != nil {
		t.Fatal(err)
	}
	m.Date = real // same folder+uid re-fetched with the corrected date
	if err := s.UpsertMessage(m); err != nil {
		t.Fatal(err)
	}
	got, err := s.MessageByFolderUID(f, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Date.Equal(real) {
		t.Fatalf("date not refreshed on conflict: got %s want %s", got.Date, real)
	}
}

// TestThreadMessagesClosure: the reading pane's conversation must reach beyond
// the inbox list window — the user's Sent reply and an older read member — and
// must collapse Gmail's INBOX/All Mail/Important copies of one message into a
// single row, preferring the inbox/sent copy.
func TestThreadMessagesClosure(t *testing.T) {
	s := open(t)
	inbox, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	sent, _ := s.UpsertFolder("A", "Sent", "sent", 1)
	all, _ := s.UpsertFolder("A", "All Mail", "archive", 2)
	other, _ := s.UpsertFolder("B", "INBOX", "inbox", 0)

	day := func(d int) time.Time { return time.Date(2026, 7, d, 0, 0, 0, 0, time.UTC) }
	put := func(acct string, folder int64, uid uint32, msgID, inReplyTo, refs string, d int) {
		if err := s.UpsertMessage(&Message{Account: acct, FolderID: folder, UID: uid, MessageID: msgID,
			InReplyTo: inReplyTo, Refs: refs, Subject: "Switching your API", Date: day(d)}); err != nil {
			t.Fatal(err)
		}
	}
	put("A", inbox, 1, "root@x", "", "", 15) // read, older than any window
	put("A", all, 2, "root@x", "", "", 15)   // Gmail duplicate
	put("A", sent, 3, "reply@x", "root@x", "root@x", 16)
	put("A", all, 4, "reply@x", "root@x", "root@x", 16) // Gmail duplicate
	put("A", inbox, 5, "last@x", "reply@x", "root@x reply@x", 17)
	put("B", other, 6, "root@x", "", "", 15) // same message-id, other account: NOT a Gmail copy

	seed, err := s.MessageByFolderUID(inbox, 5)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ThreadMessages(seed)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, m := range msgs {
		key := m.Account + "/" + m.MessageID
		if _, dup := got[key]; dup {
			t.Errorf("duplicate copy of %s in closure", key)
		}
		got[key] = m.FolderID
	}
	// 3 for account A (its All Mail copies collapsed), plus B's same-id message,
	// which is a different message and must survive as its own row.
	if len(got) != 4 {
		t.Fatalf("want 4 rows, got %d: %v", len(got), got)
	}
	if got["A/root@x"] != inbox || got["A/reply@x"] != sent || got["A/last@x"] != inbox || got["B/root@x"] != other {
		t.Errorf("dedup kept the wrong copies: %v (inbox=%d sent=%d all=%d other=%d)", got, inbox, sent, all, other)
	}
}

// TestConversationSizes: a list row must show the WHOLE conversation's size the
// way gmail.com does — the Sent reply counts, Gmail's All Mail/Important copies
// of one message do not — even though the list itself only ever sees the inbox
// members.
func TestConversationSizes(t *testing.T) {
	s := open(t)
	inbox, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	sent, _ := s.UpsertFolder("A", "Sent", "sent", 1)
	all, _ := s.UpsertFolder("A", "All Mail", "archive", 2)
	other, _ := s.UpsertFolder("B", "INBOX", "inbox", 0)

	day := func(d int) time.Time { return time.Date(2026, 7, d, 0, 0, 0, 0, time.UTC) }
	put := func(acct string, folder int64, uid uint32, msgID, inReplyTo, refs string, d int) {
		if err := s.UpsertMessage(&Message{Account: acct, FolderID: folder, UID: uid, MessageID: msgID,
			InReplyTo: inReplyTo, Refs: refs, Subject: "Switching your API", Date: day(d)}); err != nil {
			t.Fatal(err)
		}
	}
	put("A", inbox, 1, "root@x", "", "", 15)
	put("A", all, 2, "root@x", "", "", 15) // Gmail duplicate: same message
	put("A", sent, 3, "reply@x", "root@x", "root@x", 16)
	put("A", inbox, 4, "last@x", "reply@x", "root@x reply@x", 17)
	put("A", inbox, 5, "alone@x", "", "", 18) // unrelated single message
	put("B", other, 6, "root@x", "", "", 15)  // other account: its own conversation

	sizes, err := s.ConversationSizes()
	if err != nil {
		t.Fatal(err)
	}
	id := func(folder int64, uid uint32) int64 {
		m, err := s.MessageByFolderUID(folder, uid)
		if err != nil || m == nil {
			t.Fatalf("message %d/%d missing: %v", folder, uid, err)
		}
		return m.ID
	}
	// The list row is the conversation's latest inbox message ("last@x"): 4 —
	// A's three distinct messages (the All Mail copy collapsed) plus B's row,
	// exactly what ThreadMessages puts in the reading pane — not the 2 members
	// the inbox-scoped list can see.
	if got := sizes[id(inbox, 4)]; got != 4 {
		t.Errorf("conversation size = %d, want 4", got)
	}
	if got := sizes[id(inbox, 1)]; got != 4 {
		t.Errorf("size seen from the root message = %d, want 4", got)
	}
	if got := sizes[id(inbox, 5)]; got != 1 {
		t.Errorf("standalone message size = %d, want 1", got)
	}
	if got := sizes[id(other, 6)]; got != 4 {
		t.Errorf("other account's same-id message size = %d, want 4", got)
	}
	if got := sizes[id(all, 2)]; got != 4 {
		t.Errorf("size seen from the All Mail copy = %d, want 4", got)
	}
}
