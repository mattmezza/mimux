package store

import (
	"bytes"
	"testing"
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
