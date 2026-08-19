// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"sort"
	"testing"
)

// syncedNames is the account's continuously-synced folder set, by name.
func syncedNames(t *testing.T, s *Store, account string) []string {
	t.Helper()
	fs, err := s.SyncedFolders(account)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fs))
	for _, f := range fs {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return names
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSyncedFoldersDefault: a freshly discovered account syncs inbox, sent and
// drafts continuously — the three folders another client writes to — and
// nothing else.
func TestSyncedFoldersDefault(t *testing.T) {
	s := open(t)
	for _, f := range []struct{ name, special string }{
		{"INBOX", "inbox"}, {"Sent", "sent"}, {"Drafts", "drafts"},
		{"Archive", "archive"}, {"Trash", "trash"}, {"Receipts", ""},
	} {
		if _, err := s.UpsertFolder("acct", f.name, f.special, 0); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"Drafts", "INBOX", "Sent"}
	if got := syncedNames(t, s, "acct"); !eq(got, want) {
		t.Errorf("default synced set = %v, want %v", got, want)
	}
}

// TestDeselectSurvivesReLIST is the whole reason UpsertFolder writes sync on
// INSERT only: every reconnect re-LISTs the mailboxes, and that must not undo a
// choice made in Settings.
func TestDeselectSurvivesReLIST(t *testing.T) {
	s := open(t)
	sent, err := s.UpsertFolder("acct", "Sent", "sent", 0)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := s.UpsertFolder("acct", "INBOX", "inbox", 0)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := s.UpsertFolder("acct", "Archive", "archive", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The user unticks Sent and ticks Archive.
	if err := s.SetSyncedFolders("acct", []int64{inbox, archive}); err != nil {
		t.Fatal(err)
	}
	// A reconnect: same LIST, same upserts.
	for _, f := range []struct {
		name, special string
	}{{"Sent", "sent"}, {"INBOX", "inbox"}, {"Archive", "archive"}} {
		if _, err := s.UpsertFolder("acct", f.name, f.special, 0); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"Archive", "INBOX"}
	if got := syncedNames(t, s, "acct"); !eq(got, want) {
		t.Errorf("synced set after re-LIST = %v, want %v (id %d should stay off)", got, want, sent)
	}
}

// TestInboxAlwaysSyncs: the SQL ORs the inbox back in, so no settings post can
// switch off the one folder the unread badge and notifications assume is fresh.
func TestInboxAlwaysSyncs(t *testing.T) {
	s := open(t)
	if _, err := s.UpsertFolder("acct", "INBOX", "inbox", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSyncedFolders("acct", nil); err != nil {
		t.Fatal(err)
	}
	if got := syncedNames(t, s, "acct"); !eq(got, []string{"INBOX"}) {
		t.Errorf("synced set = %v, want the inbox regardless", got)
	}
}

// TestSetSyncedFoldersIsPerAccount: saving one account's set leaves another's
// alone (both the clear and the set are scoped).
func TestSetSyncedFoldersIsPerAccount(t *testing.T) {
	s := open(t)
	if _, err := s.UpsertFolder("a", "Sent", "sent", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertFolder("b", "Sent", "sent", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSyncedFolders("a", nil); err != nil {
		t.Fatal(err)
	}
	if got := syncedNames(t, s, "b"); !eq(got, []string{"Sent"}) {
		t.Errorf("account b's synced set = %v, want [Sent] untouched", got)
	}
}
