// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/mail"
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

// composeUpload builds a multipart compose POST carrying one file, the shape
// the browser sends when something is attached.
func composeUpload(t *testing.T, path string, fields url.Values, filename, content string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, vs := range fields {
		for _, v := range vs {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	if filename != "" {
		w, err := mw.CreateFormFile("attachments", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", path, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

// TestDraftKeepsItsAttachments is the whole point of draft attachments: a file
// picked in compose is still there after the window is closed and the draft
// reopened, and removing it removes it for good.
func TestDraftKeepsItsAttachments(t *testing.T) {
	s := testServer(t)
	fields := url.Values{
		"draft_id": {"0"}, "account": {"Personal"}, "to": {"ada@example.com"},
		"subject": {"the numbers"}, "body": {"see attached"}, "kind": {"new"}, "mode": {"plain"},
	}
	rec := httptest.NewRecorder()
	s.handleComposeDraftSave(rec, composeUpload(t, "/compose/draft", fields, "numbers.txt", "41,42,43"))

	drafts, err := s.store.ListDrafts()
	if err != nil || len(drafts) != 1 {
		t.Fatalf("ListDrafts = %v, %v", drafts, err)
	}
	id := drafts[0].ID
	atts, err := s.store.DraftAttachments(id)
	if err != nil || len(atts) != 1 || string(atts[0].Data) != "41,42,43" {
		t.Fatalf("stored attachments = %+v, %v, want the uploaded file", atts, err)
	}
	// The save answers with the draft's own chip row, so the window stops
	// showing the file as still-unsaved.
	if body := rec.Body.String(); !strings.Contains(body, "numbers.txt") || !strings.Contains(body, "hx-swap-oob") {
		t.Errorf("save response = %q, want an out-of-band chip row", body)
	}

	// Reopening lists it, with a way to remove it.
	rec = httptest.NewRecorder()
	s.handleComposeNew(rec, httptest.NewRequest("GET", "/compose?draft="+strconv.FormatInt(id, 10), nil))
	body := rec.Body.String()
	if !strings.Contains(body, "numbers.txt") {
		t.Errorf("reopened compose does not list the attachment: %q", body)
	}
	del := "/compose/draft/" + strconv.FormatInt(id, 10) + "/attachment/" + strconv.FormatInt(atts[0].ID, 10) + "/delete"
	if !strings.Contains(body, del) {
		t.Errorf("reopened compose has no remove control for the attachment")
	}

	// Removing it re-owes the push, so the mailbox copy loses it too.
	if err := s.store.ClearDraftDirty(id, "x@example.com", 0, 0, drafts[0].UpdatedAt); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	router := chi.NewRouter()
	router.Post("/compose/draft/{id}/attachment/{aid}/delete", s.handleDraftAttachmentDelete)
	router.ServeHTTP(rec, httptest.NewRequest("POST", del, nil))
	if left, _ := s.store.DraftAttachments(id); len(left) != 0 {
		t.Errorf("%d attachment(s) left after removal", len(left))
	}
	got, _ := s.store.DraftByID(id)
	if got == nil || !got.IMAPDirty {
		t.Errorf("after removing a file the draft = %+v, want it owing a fresh push", got)
	}
}

// TestSendAttachesTheDraftsFiles: what the draft kept goes out with it, in
// front of anything picked since the last save.
func TestSendAttachesTheDraftsFiles(t *testing.T) {
	s := testServer(t)
	s.cfg.Accounts = []config.Account{{Name: "Personal", Email: "me@example.com"}}

	d := &store.Draft{Account: "Personal", To: "ada@example.com", Subject: "hi", Kind: "new"}
	if err := s.store.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}
	if err := s.store.AddDraftAttachment(d.ID, &store.DraftAttachment{
		Filename: "kept.txt", ContentType: "text/plain", Data: []byte("from the draft"),
	}); err != nil {
		t.Fatal(err)
	}

	fields := url.Values{
		"draft_id": {strconv.FormatInt(d.ID, 10)}, "from": {"me@example.com"},
		"to": {"ada@example.com"}, "subject": {"hi"}, "body": {"there"},
		"kind": {"new"}, "mode": {"plain"}, "send_mode": {"later"},
	}
	rec := httptest.NewRecorder()
	s.handleComposeSend(rec, composeUpload(t, "/compose", fields, "fresh.txt", "picked just now"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("send status = %d: %s", rec.Code, rec.Body.String())
	}

	queued, err := s.store.DueOutbox(time.Now().Add(time.Hour))
	if err != nil || len(queued) != 1 {
		t.Fatalf("outbox = %v, %v, want the queued message", queued, err)
	}
	names := []string{}
	for _, a := range queued[0].Attachments {
		names = append(names, a.Filename)
	}
	if len(names) != 2 || names[0] != "kept.txt" || names[1] != "fresh.txt" {
		t.Fatalf("attachments sent = %v, want the draft's file then the new one", names)
	}
	// Sending drops the draft, and its files go with it.
	if left, _ := s.store.DraftAttachments(d.ID); len(left) != 0 {
		t.Errorf("%d attachment(s) outlived the sent draft", len(left))
	}
}

// TestDraftAttachmentsRespectTheSendCap: the drafts path answers to the same
// 25MB total as a send, counting what is already kept.
func TestDraftAttachmentsRespectTheSendCap(t *testing.T) {
	s := testServer(t)
	d := &store.Draft{Account: "Personal", Subject: "big", Kind: "new"}
	if err := s.store.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}
	if err := s.store.AddDraftAttachment(d.ID, &store.DraftAttachment{
		Filename: "already.bin", Data: make([]byte, maxAttachTotal-10),
	}); err != nil {
		t.Fatal(err)
	}
	msg := s.keepDraftAttachments(d.ID, []mail.OutAttachment{
		{Filename: "one-too-many.bin", Data: make([]byte, 11)},
	})
	if msg == "" {
		t.Fatal("a file that busts the cap was kept with the draft")
	}
	if got, _ := s.store.DraftAttachments(d.ID); len(got) != 1 {
		t.Errorf("draft holds %d files, want only the one that fit", len(got))
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

// TestDraftRowsMergeLocalAndMailbox: the page is one list. A local draft and
// its own published copy are the same message and must appear once; a draft
// written in another client has no local row and still has to show up.
func TestDraftRowsMergeLocalAndMailbox(t *testing.T) {
	s := testServer(t)
	folderID, err := s.store.UpsertFolder("Personal", "Drafts", "drafts", 2)
	if err != nil {
		t.Fatal(err)
	}
	seed := func(uid uint32, msgID, subject string) {
		t.Helper()
		if err := s.store.UpsertMessage(&store.Message{
			Account: "Personal", FolderID: folderID, UID: uid, MessageID: msgID,
			Subject: subject, Date: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed(1, "mine@example.com", "written here")
	seed(2, "theirs@example.com", "written on the phone")

	d := &store.Draft{Account: "Personal", Subject: "written here", Kind: "new"}
	if err := s.store.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}
	// Published: this is the row the synced copy belongs to.
	if err := s.store.ClearDraftDirty(d.ID, "mine@example.com", folderID, 1, d.UpdatedAt); err != nil {
		t.Fatal(err)
	}

	rows, err := s.draftRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("draftRows = %d rows (%+v), want 2 — the local draft and its copy are one item", len(rows), rows)
	}
	var editable, foreign int
	for _, r := range rows {
		if r.Edit == d.ID {
			editable++
		}
		if r.Edit == 0 && r.Subject == "written on the phone" && r.Message != 0 {
			foreign++
		}
	}
	if editable != 1 {
		t.Errorf("the local draft appears %d times, want once", editable)
	}
	if foreign != 1 {
		t.Errorf("the other client's draft appears %d times, want once and read-only", foreign)
	}
}
