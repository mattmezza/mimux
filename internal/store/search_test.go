// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"testing"
	"time"

	"github.com/mattmezza/mimux/internal/search"
)

func seedMsg(t *testing.T, s *Store, folder int64, uid uint32, from, subj, snip string) int64 {
	t.Helper()
	m := &Message{
		Account: "A", FolderID: folder, UID: uid, MessageID: subj,
		FromAddress: from, FromName: from, Subject: subj, Snippet: snip,
		Date: time.Now(),
	}
	if err := s.UpsertMessage(m); err != nil {
		t.Fatal(err)
	}
	got, _ := s.MessageByFolderUID(folder, uid)
	return got.ID
}

// TestFTSEndToEnd inserts messages, matches via SearchLocal, then verifies the
// index stays consistent across update and delete (trigger correctness).
func TestFTSEndToEnd(t *testing.T) {
	s := open(t)
	f, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	id := seedMsg(t, s, f, 1, "alice@x.com", "Deploy plan", "ship it friday")
	seedMsg(t, s, f, 2, "bob@x.com", "Lunch", "tacos at noon")

	find := func(q string) []Message {
		res, err := s.SearchLocal(search.Parse(q), search.ScopeAll, "", 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if got := find("deploy"); len(got) != 1 || got[0].Subject != "Deploy plan" {
		t.Fatalf("free-text: got %d rows", len(got))
	}
	if got := find("subject:lunch"); len(got) != 1 || got[0].Subject != "Lunch" {
		t.Fatalf("subject: got %d rows", len(got))
	}
	if got := find("from:alice"); len(got) != 1 {
		t.Fatalf("from: got %d rows", len(got))
	}
	if got := find("tacos"); len(got) != 1 {
		t.Fatalf("snippet free-text: got %d rows", len(got))
	}

	// Update the subject: the old token must no longer match, the new one must.
	if err := s.DB.QueryRow(`SELECT id FROM messages WHERE id = ?`, id).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE messages SET subject = 'Rollback plan' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if got := find("deploy"); len(got) != 0 {
		t.Fatalf("after update, stale token still matches: %d rows", len(got))
	}
	if got := find("rollback"); len(got) != 1 {
		t.Fatalf("after update, new token missing: %d rows", len(got))
	}

	// Delete: the row leaves the index.
	if err := s.DeleteMessage(id); err != nil {
		t.Fatal(err)
	}
	if got := find("rollback"); len(got) != 0 {
		t.Fatalf("after delete, token still matches: %d rows", len(got))
	}
}

func TestSearchLocalPredicates(t *testing.T) {
	s := open(t)
	f, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	unread := seedMsg(t, s, f, 1, "a@x.com", "one", "")
	_ = seedMsg(t, s, f, 2, "b@x.com", "two", "")
	_ = s.SetRead(unread, false)
	_, _ = s.DB.Exec(`UPDATE messages SET is_read = 1 WHERE uid = 2`)

	res, err := s.SearchLocal(search.Parse("is:unread"), search.ScopeAll, "", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].ID != unread {
		t.Fatalf("is:unread returned %d rows", len(res))
	}
}

func TestSearchCacheRoundTrip(t *testing.T) {
	s := open(t)
	h := QueryHash("from:alice", search.ScopeAll, "", 0)
	if _, ok := s.SearchCacheGet(h, time.Minute); ok {
		t.Fatal("empty cache returned a hit")
	}
	if err := s.SearchCachePut(h, []int64{3, 1, 2}); err != nil {
		t.Fatal(err)
	}
	ids, ok := s.SearchCacheGet(h, time.Minute)
	if !ok || len(ids) != 3 || ids[0] != 3 || ids[2] != 2 {
		t.Fatalf("cache round-trip = %v ok=%v", ids, ok)
	}
	if _, ok := s.SearchCacheGet(h, -time.Minute); ok {
		t.Fatal("expired entry returned a hit")
	}
}

func TestSearchHistoryAndSaved(t *testing.T) {
	s := open(t)
	for _, q := range []string{"a", "b", "a"} { // "a" repeated collapses to newest
		if err := s.AddHistory(q); err != nil {
			t.Fatal(err)
		}
	}
	hist, _ := s.ListHistory(10)
	if len(hist) != 2 || hist[0] != "a" || hist[1] != "b" {
		t.Fatalf("history = %v", hist)
	}
	if err := s.ClearHistory(); err != nil {
		t.Fatal(err)
	}
	if hist, _ := s.ListHistory(10); len(hist) != 0 {
		t.Fatalf("history not cleared: %v", hist)
	}

	if err := s.SaveSearch("Starred", "is:starred"); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.ListSavedSearches()
	if len(saved) != 1 || saved[0].Name != "Starred" || saved[0].Query != "is:starred" {
		t.Fatalf("saved = %v", saved)
	}
	if err := s.DeleteSavedSearch(saved[0].ID); err != nil {
		t.Fatal(err)
	}
	if saved, _ := s.ListSavedSearches(); len(saved) != 0 {
		t.Fatalf("saved not deleted: %v", saved)
	}
}
