package mail

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mattmezza/mimux/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedFolderWithMessage(t *testing.T, st *store.Store, uidvalidity uint32) *store.Folder {
	t.Helper()
	id, err := st.UpsertFolder("acct", "INBOX", "inbox", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetFolderUIDValidity(id, uidvalidity); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMessage(&store.Message{
		Account: "acct", FolderID: id, UID: 42, Subject: "hi", Date: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	f, err := st.FolderByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestUIDValidityChangeResetsCache(t *testing.T) {
	st := testStore(t)
	f := seedFolderWithMessage(t, st, 1000)

	reset, err := syncUIDValidity(st, f, 2000) // server changed UIDVALIDITY
	if err != nil {
		t.Fatal(err)
	}
	if !reset {
		t.Fatal("expected a cache reset on UIDVALIDITY change")
	}
	uids, _ := st.FolderUIDs(f.ID)
	if len(uids) != 0 {
		t.Errorf("messages should be cleared, got %d", len(uids))
	}
	if f.UIDValidity != 2000 {
		t.Errorf("in-memory folder should track new UIDVALIDITY, got %d", f.UIDValidity)
	}
	reloaded, _ := st.FolderByID(f.ID)
	if reloaded.UIDValidity != 2000 {
		t.Errorf("stored UIDVALIDITY not updated, got %d", reloaded.UIDValidity)
	}
}

func TestUIDValidityUnchangedKeepsCache(t *testing.T) {
	st := testStore(t)
	f := seedFolderWithMessage(t, st, 1000)

	reset, err := syncUIDValidity(st, f, 1000) // same value
	if err != nil {
		t.Fatal(err)
	}
	if reset {
		t.Fatal("no reset expected when UIDVALIDITY is unchanged")
	}
	uids, _ := st.FolderUIDs(f.ID)
	if len(uids) != 1 {
		t.Errorf("messages should be kept, got %d", len(uids))
	}
}

func TestUIDValidityFirstSyncNoReset(t *testing.T) {
	st := testStore(t)
	id, _ := st.UpsertFolder("acct", "INBOX", "inbox", 0) // uidvalidity 0 = never synced
	f, _ := st.FolderByID(id)

	reset, err := syncUIDValidity(st, f, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if reset {
		t.Error("first sync (uidvalidity 0) should not count as a reset")
	}
	if f.UIDValidity != 5000 {
		t.Errorf("UIDVALIDITY should be recorded, got %d", f.UIDValidity)
	}
}
