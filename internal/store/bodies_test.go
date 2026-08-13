package store

import (
	"bytes"
	"testing"
	"time"
)

// TestMessageBodyRoundTrip covers Save/Get/Delete and the ON DELETE CASCADE
// cleanup when the owning message row is deleted.
func TestMessageBodyRoundTrip(t *testing.T) {
	s := open(t)
	f, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	id := seedMsg(t, s, f, 1, "alice@x.com", "hi", "snip")

	if _, ok, _ := s.GetMessageBody(id); ok {
		t.Fatal("body present before save")
	}

	blob := []byte{0, 1, 2, 3, 255}
	if err := s.SaveMessageBody(id, blob); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetMessageBody(id)
	if err != nil || !ok || !bytes.Equal(got, blob) {
		t.Fatalf("GetMessageBody = %v, %v, %v", got, ok, err)
	}

	// Overwrite (re-fetch path).
	blob2 := []byte{9, 9}
	if err := s.SaveMessageBody(id, blob2); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetMessageBody(id); !bytes.Equal(got, blob2) {
		t.Fatalf("overwrite failed: %v", got)
	}

	if err := s.DeleteMessageBody(id); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetMessageBody(id); ok {
		t.Fatal("body present after delete")
	}

	// Cascade: deleting the message drops its cached body.
	_ = s.SaveMessageBody(id, blob)
	if err := s.DeleteMessage(id); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetMessageBody(id); ok {
		t.Fatal("body survived message deletion (cascade broken)")
	}
}

// TestMessagesWithoutBody: the warmer's work queue must return only this
// folder's uncached messages, newest first, capped by the limit.
func TestMessagesWithoutBody(t *testing.T) {
	s := open(t)
	inbox, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	other, _ := s.UpsertFolder("A", "Archive", "archive", 3)

	day := func(d int) time.Time { return time.Date(2026, 7, d, 0, 0, 0, 0, time.UTC) }
	put := func(folder int64, uid uint32, subj string, d int) int64 {
		if err := s.UpsertMessage(&Message{Account: "A", FolderID: folder, UID: uid,
			MessageID: subj, Subject: subj, Date: day(d)}); err != nil {
			t.Fatal(err)
		}
		m, _ := s.MessageByFolderUID(folder, uid)
		return m.ID
	}
	put(inbox, 1, "oldest", 1)
	cached := put(inbox, 2, "cached", 2)
	put(inbox, 3, "middle", 3)
	put(inbox, 4, "newest", 4)
	put(other, 5, "elsewhere", 9) // different folder: never warmed
	if err := s.SaveMessageBody(cached, []byte("blob")); err != nil {
		t.Fatal(err)
	}

	got, err := s.MessagesWithoutBody(inbox, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"newest", "middle", "oldest"}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Subject != want[i] {
			t.Fatalf("row %d = %q, want %q", i, got[i].Subject, want[i])
		}
	}

	limited, err := s.MessagesWithoutBody(inbox, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 || limited[0].Subject != "newest" || limited[1].Subject != "middle" {
		t.Fatalf("limit ignored: %+v", limited)
	}
}
