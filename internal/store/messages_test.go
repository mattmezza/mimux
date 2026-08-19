// SPDX-License-Identifier: AGPL-3.0-only
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

// TestListMessagesPage: the list pages by conversation, so the three ways
// row-based paging goes wrong can't happen — a message repeated on two pages, a
// conversation cut in half by the boundary (BuildThreads would render it as two
// half-rows), and a message arriving between two fetches skipping a row the way
// LIMIT/OFFSET does.
func TestListMessagesPage(t *testing.T) {
	s := open(t)
	inbox, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	day := func(d int) time.Time { return time.Date(2026, 7, d, 0, 0, 0, 0, time.UTC) }
	// Read: page one carries every unread conversation however old, which would
	// leave nothing for the later pages to hold.
	put := func(uid uint32, msgID, refs string, d int) int64 {
		if err := s.UpsertMessage(&Message{Account: "A", FolderID: inbox, UID: uid, MessageID: msgID,
			Refs: refs, Subject: "Switching your API", Date: day(d), IsRead: true}); err != nil {
			t.Fatal(err)
		}
		m, err := s.MessageByFolderUID(inbox, uid)
		if err != nil || m == nil {
			t.Fatalf("message %d missing: %v", uid, err)
		}
		return m.ID
	}
	// Five conversations, newest member first: a(20), b(19 and 5 — the straddler
	// a row-based page boundary would cut), c(18), d(6), e(4).
	a := put(1, "a@x", "", 20)
	bOld := put(2, "b1@x", "", 5)
	bNew := put(3, "b2@x", "b1@x", 19)
	c := put(4, "c@x", "", 18)
	d := put(5, "d@x", "", 6)
	e := put(6, "e@x", "", 4)

	page := func(before string) ([]int64, string) {
		t.Helper()
		msgs, next, err := s.ListMessagesPage(inbox, before, 2) // 2 conversations a page
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]int64, 0, len(msgs))
		for _, m := range msgs {
			ids = append(ids, m.ID)
		}
		return ids, next
	}
	has := func(ids []int64, id int64) bool {
		for _, got := range ids {
			if got == id {
				return true
			}
		}
		return false
	}

	p1, cur1 := page("")
	if !has(p1, bNew) || !has(p1, bOld) {
		t.Fatalf("conversation split across the page boundary: page 1 = %v, want both %d and %d", p1, bNew, bOld)
	}
	if !has(p1, a) || len(p1) != 3 {
		t.Fatalf("page 1 = %v, want exactly the two newest conversations (%d + %d,%d)", p1, a, bNew, bOld)
	}
	if cur1 == "" {
		t.Fatal("page 1 handed out no cursor, so the list can never reach page 2")
	}

	// A message arriving between the two fetches must not push a row past the
	// cursor and out of sight — the failure mode LIMIT/OFFSET has.
	put(7, "new@x", "", 30)

	p2, cur2 := page(cur1)
	if len(p2) != 2 || !has(p2, c) || !has(p2, d) {
		t.Fatalf("page 2 = %v, want exactly %d and %d", p2, c, d)
	}
	p3, cur3 := page(cur2)
	if len(p3) != 1 || !has(p3, e) {
		t.Fatalf("page 3 = %v, want just %d", p3, e)
	}
	if cur3 != "" {
		t.Errorf("last page still offers a cursor (%q): the load-more sentinel would never disappear", cur3)
	}
	seen := map[int64]bool{}
	for _, ids := range [][]int64{p1, p2, p3} {
		for _, id := range ids {
			if seen[id] {
				t.Errorf("message %d rendered on two pages", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 6 { // the day-30 arrival belongs to page one, which is already on screen
		t.Errorf("pages covered %d of the 6 stored messages: %v", len(seen), seen)
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

// TestLabelsSurviveResync: user labels live in the same column sync writes, and
// sync hands us "" for every message it can't read X-GM-LABELS for (today: all
// of them). An empty incoming value must not clobber what the user applied.
func TestLabelsSurviveResync(t *testing.T) {
	s := open(t)
	f, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	m := &Message{Account: "A", FolderID: f, UID: 1, Subject: "hi", Date: time.Now().UTC()}
	if err := s.UpsertMessage(m); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.MessageByFolderUID(f, 1)
	if _, err := s.SetLabels(stored.ID, "Work Project_X"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMessage(m); err != nil { // same UID re-synced, Labels still ""
		t.Fatal(err)
	}
	got, err := s.MessageByFolderUID(f, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels != "Work Project_X" {
		t.Fatalf("labels after re-sync = %q, want %q", got.Labels, "Work Project_X")
	}
	// A real Gmail label set, once the client can fetch one, still wins.
	m.Labels = `\Inbox Work`
	if err := s.UpsertMessage(m); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.MessageByFolderUID(f, 1); got.Labels != `\Inbox Work` {
		t.Fatalf("server labels not applied: %q", got.Labels)
	}
	raws, err := s.DistinctLabels()
	if err != nil || len(raws) != 1 || raws[0] != `\Inbox Work` {
		t.Fatalf("DistinctLabels = %v, %v", raws, err)
	}
}

// TestSettersReportNoOp: the sync path tells "the server changed something"
// from "the server is echoing back what mimux just wrote" by whether the write
// moved a row. A setter called with the value already stored must say so.
func TestSettersReportNoOp(t *testing.T) {
	s := open(t)
	f, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	if err := s.UpsertMessage(&Message{Account: "A", FolderID: f, UID: 1, Subject: "hi", Date: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	m, _ := s.MessageByFolderUID(f, 1)

	for _, tc := range []struct {
		name string
		set  func() (bool, error)
	}{
		{"SetReadFromServer", func() (bool, error) { return s.SetReadFromServer(m.ID, true) }},
		{"SetStarred", func() (bool, error) { return s.SetStarred(m.ID, true) }},
		{"SetLabels", func() (bool, error) { return s.SetLabels(m.ID, "Work") }},
	} {
		got, err := tc.set()
		if err != nil || !got {
			t.Fatalf("%s first write = %v, %v; want true", tc.name, got, err)
		}
		got, err = tc.set()
		if err != nil || got {
			t.Errorf("%s re-write of the same value = %v, %v; want false", tc.name, got, err)
		}
	}
}
