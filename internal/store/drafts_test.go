// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"testing"
	"time"
)

func TestDraftsCRUD(t *testing.T) {
	s := open(t)

	if drafts, err := s.ListDrafts(); err != nil || len(drafts) != 0 {
		t.Fatalf("fresh db: drafts=%v err=%v", drafts, err)
	}

	d := &Draft{Account: "work", To: "a@x", Subject: "Hi", Body: "Hello", Kind: "new"}
	if err := s.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}
	if d.ID == 0 {
		t.Fatal("expected an assigned id")
	}

	got, err := s.DraftByID(d.ID)
	if err != nil || got == nil {
		t.Fatalf("DraftByID: %v, %v", got, err)
	}
	if got.Subject != "Hi" || got.Body != "Hello" || got.Account != "work" {
		t.Errorf("DraftByID = %+v", got)
	}

	// Update in place: same id, no duplicate row.
	d.Body = "Hello, updated"
	if err := s.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}
	if drafts, err := s.ListDrafts(); err != nil || len(drafts) != 1 {
		t.Fatalf("ListDrafts after update = %v, %v, want exactly 1", drafts, err)
	}
	got, _ = s.DraftByID(d.ID)
	if got.Body != "Hello, updated" {
		t.Errorf("Body after update = %q", got.Body)
	}

	if err := s.DeleteDraft(d.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DraftByID(d.ID); err != nil || got != nil {
		t.Errorf("DraftByID after delete = %v, %v, want nil", got, err)
	}
}

// TestDraftIMAPDirty is the write-through cache contract: a save owes a push, a
// completed push clears the debt and records where the revision landed, and a
// save that raced the push leaves the debt standing.
func TestDraftIMAPDirty(t *testing.T) {
	s := open(t)

	d := &Draft{Account: "work", To: "a@x", Subject: "Hi", Kind: "new"}
	if err := s.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}
	dirty, err := s.DirtyDrafts("work", 10)
	if err != nil || len(dirty) != 1 || dirty[0].ID != d.ID {
		t.Fatalf("DirtyDrafts after save = %v, %v, want the new draft", dirty, err)
	}
	if !dirty[0].IMAPDirty {
		t.Error("a freshly saved draft is not marked dirty")
	}

	// The push lands: location recorded, debt cleared.
	if err := s.ClearDraftDirty(d.ID, "abc@example.com", 7, 42, dirty[0].UpdatedAt); err != nil {
		t.Fatal(err)
	}
	got, _ := s.DraftByID(d.ID)
	if got.MessageID != "abc@example.com" || got.FolderID != 7 || got.UID != 42 || got.IMAPDirty {
		t.Fatalf("after ClearDraftDirty = %+v", got)
	}
	if dirty, _ := s.DirtyDrafts("work", 10); len(dirty) != 0 {
		t.Errorf("still %d dirty draft(s) after a clean push", len(dirty))
	}

	// Editing again re-owes the push and keeps the recorded location — the next
	// revision has to expunge that copy.
	d.Body = "more"
	if err := s.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}
	got, _ = s.DraftByID(d.ID)
	if !got.IMAPDirty || got.UID != 42 || got.MessageID != "abc@example.com" {
		t.Fatalf("after re-save = %+v", got)
	}

	// A push that finished against a stale revision must not clear the debt.
	if err := s.ClearDraftDirty(d.ID, "abc@example.com", 7, 43, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.DraftByID(d.ID); !got.IMAPDirty {
		t.Error("a push that raced a save cleared the dirty flag")
	}
	if got.UID != 43 {
		t.Errorf("UID = %d, want the raced push's copy recorded so it can be expunged", got.UID)
	}
}
