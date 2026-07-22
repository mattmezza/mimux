package store

import "testing"

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
