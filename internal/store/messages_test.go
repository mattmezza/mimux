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
