// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mattmezza/mimux/internal/store"
)

// TestDraftSaveIsLocalFirst: the save endpoint writes SQLite and leaves the row
// owing an IMAP push. That ordering is the whole safety story — the publish runs
// in the background and a server that is down costs a retry, never the draft.
func TestDraftSaveIsLocalFirst(t *testing.T) {
	s := testServer(t)
	form := url.Values{
		"draft_id": {"0"}, "account": {"Personal"}, "to": {"ada@example.com"},
		"subject": {"Hi"}, "body": {"half a thought"}, "kind": {"new"}, "mode": {"plain"},
	}
	r := httptest.NewRequest("POST", "/compose/draft", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleComposeDraftSave(httptest.NewRecorder(), r)

	drafts, err := s.store.ListDrafts()
	if err != nil || len(drafts) != 1 {
		t.Fatalf("ListDrafts = %v, %v, want the one saved draft", drafts, err)
	}
	if drafts[0].Body != "half a thought" {
		t.Errorf("Body = %q", drafts[0].Body)
	}
	if !drafts[0].IMAPDirty {
		t.Error("the saved draft does not owe an IMAP push: it will never reach the Drafts folder")
	}

	// Sending or discarding takes the local row with it (the mailbox copy goes
	// in the background, see Manager.DropDraft).
	s.dropDraft(drafts[0].ID)
	if left, _ := s.store.ListDrafts(); len(left) != 0 {
		t.Errorf("%d draft(s) left after dropDraft", len(left))
	}
}

// TestDropDraftIgnoresNothing: send with no draft open, and a delete of a row
// that is already gone, must both be no-ops rather than errors.
func TestDropDraftIgnoresNothing(t *testing.T) {
	s := testServer(t)
	s.dropDraft(0)
	s.dropDraft(999)

	d := &store.Draft{Account: "Personal", Subject: "Hi", Kind: "new"}
	if err := s.store.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}
	s.dropDraft(d.ID)
	s.dropDraft(d.ID)
	if left, _ := s.store.ListDrafts(); len(left) != 0 {
		t.Errorf("%d draft(s) left", len(left))
	}
}
