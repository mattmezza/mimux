// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

// draftThreadRouter wires the routes a draft-in-a-thread scenario needs: the
// folder/unified lists, the thread pane and its inline sub-row disclosure.
func draftThreadRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/u", s.handleUnified)
	r.Get("/f/{id}", s.handleFolder)
	r.Get("/t/{id}", s.handleThread)
	r.Get("/t/{id}/rows", s.handleThreadRows)
	return r
}

// seedDraftThread builds a two-message conversation split across an inbox and
// its account's Drafts folder — the "draft reply, not sent yet" shape the
// thread view has to render: a root message the user received, and a draft
// reply to it that Message-ID/In-Reply-To threading pulls into the same
// conversation across folders (mail.Manager.Conversation, not the per-folder
// list scope). Returns the root message's id and the draft copy's message row.
func seedDraftThread(t *testing.T, s *Server) (rootID int64, draftMsg *store.Message, inboxID, draftsFolderID int64) {
	t.Helper()
	var err error
	inboxID, err = s.store.UpsertFolder("Personal", "INBOX", "inbox", 0)
	if err != nil {
		t.Fatal(err)
	}
	draftsFolderID, err = s.store.UpsertFolder("Personal", "Drafts", "drafts", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertMessage(&store.Message{
		Account: "Personal", FolderID: inboxID, UID: 1, MessageID: "root@x",
		Subject: "Deploy", FromName: "Ada", IsRead: true,
		Date: time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertMessage(&store.Message{
		Account: "Personal", FolderID: draftsFolderID, UID: 1, MessageID: "draft@x",
		InReplyTo: "root@x", Refs: "<root@x>", Subject: "Re: Deploy", FromName: "me",
		Date: time.Date(2026, 1, 1, 0, 20, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.store.ListMessages(inboxID, 10)
	if len(msgs) != 1 {
		t.Fatalf("seed: inbox message not found (%d rows)", len(msgs))
	}
	rootID = msgs[0].ID
	dmsgs, _ := s.store.ListMessages(draftsFolderID, 10)
	if len(dmsgs) != 1 {
		t.Fatalf("seed: draft message not found (%d rows)", len(dmsgs))
	}
	return rootID, &dmsgs[0], inboxID, draftsFolderID
}

// TestThreadDetailMarksBuriedForeignDraft: a draft reply nobody has adopted
// yet, one member of a two-folder conversation, must carry the "Draft" badge
// and a compose link that ADOPTS it (same /compose?adopt= link draftRows and
// /drafts give it) — while the message it replies to renders exactly as
// before, and the collapsed list row (whose latest message is the INBOX one,
// not the draft — a draft never surfaces in a folder-scoped list row unless
// that folder itself is Drafts) keeps opening the reading pane.
func TestThreadDetailMarksBuriedForeignDraft(t *testing.T) {
	s := serverWith(t, []config.Account{{Name: "Personal"}}, nil)
	rootID, draftMsg, inboxID, _ := seedDraftThread(t, s)
	r := draftThreadRouter(s)

	// The collapsed folder list still opens the thread normally: the row's own
	// message (root@x) is not a draft.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/f/"+strconv.FormatInt(inboxID, 10), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/f status = %d", rec.Code)
	}
	listBody := rec.Body.String()
	if !strings.Contains(listBody, "hx-get=\"/t/"+strconv.FormatInt(rootID, 10)) {
		t.Errorf("the list row for the non-draft root message must still open the thread pane:\n%s", listBody)
	}
	if strings.Contains(listBody, ">Draft<") {
		t.Errorf("the list row's own message is not a draft, no badge expected:\n%s", listBody)
	}

	// The thread pane pulls in the draft across folders and must mark it.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t/"+strconv.FormatInt(rootID, 10)+"?src="+strconv.FormatInt(inboxID, 10), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/t status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ">Draft<") {
		t.Errorf("thread detail does not badge the buried draft:\n%s", body)
	}
	adopt := `hx-get="/compose?adopt=` + strconv.FormatInt(draftMsg.ID, 10) + `"`
	if !strings.Contains(body, adopt) {
		t.Errorf("thread detail's Edit action does not adopt the foreign draft (want %q):\n%s", adopt, body)
	}
	if !strings.Contains(body, "Edit draft") {
		t.Errorf("thread detail has no Edit draft action:\n%s", body)
	}

	// The inline sub-row disclosure (list-side expand) must badge it too.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t/"+strconv.FormatInt(rootID, 10)+"/rows", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/t/.../rows status = %d", rec.Code)
	}
	rowsBody := rec.Body.String()
	if !strings.Contains(rowsBody, ">Draft<") || !strings.Contains(rowsBody, adopt) {
		t.Errorf("expanded thread sub-rows do not mark/adopt the draft:\n%s", rowsBody)
	}
}

// TestThreadDetailOpensLocalDraftDirectly: once the buried draft has a local
// row (adopted, or written here in the first place), the same message must
// link straight to /compose?draft=<local id> instead of going through adopt —
// draftRows' local-vs-foreign split, reused for the thread view.
func TestThreadDetailOpensLocalDraftDirectly(t *testing.T) {
	s := serverWith(t, []config.Account{{Name: "Personal"}}, nil)
	rootID, draftMsg, inboxID, draftsFolderID := seedDraftThread(t, s)

	d := &store.Draft{Account: "Personal", Subject: "Re: Deploy", InReplyTo: "root@x", Kind: "reply"}
	if err := s.store.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}
	if err := s.store.ClearDraftDirty(d.ID, "draft@x", draftsFolderID, 1, d.UpdatedAt); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	draftThreadRouter(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/t/"+strconv.FormatInt(rootID, 10)+"?src="+strconv.FormatInt(inboxID, 10), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/t status = %d", rec.Code)
	}
	body := rec.Body.String()
	want := `hx-get="/compose?draft=` + strconv.FormatInt(d.ID, 10) + `"`
	if !strings.Contains(body, want) {
		t.Errorf("thread detail does not open the local draft directly (want %q):\n%s", want, body)
	}
	if strings.Contains(body, "/compose?adopt="+strconv.FormatInt(draftMsg.ID, 10)) {
		t.Errorf("thread detail still offers to adopt a draft that already has a local row:\n%s", body)
	}
}

// TestDraftsFolderListRowOpensCompose: browsing an account's Drafts folder
// directly renders the same thread_row template as any other folder — its
// rows must badge themselves and open compose (edit-first, like /drafts)
// rather than the plain reading pane.
func TestDraftsFolderListRowOpensCompose(t *testing.T) {
	s := serverWith(t, []config.Account{{Name: "Personal"}}, nil)
	draftsFolderID, err := s.store.UpsertFolder("Personal", "Drafts", "drafts", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertMessage(&store.Message{
		Account: "Personal", FolderID: draftsFolderID, UID: 5, MessageID: "solo@x",
		Subject: "unsent", Date: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.store.ListMessages(draftsFolderID, 10)
	if len(msgs) != 1 {
		t.Fatalf("seed: draft not found")
	}

	rec := httptest.NewRecorder()
	draftThreadRouter(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/f/"+strconv.FormatInt(draftsFolderID, 10), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/f status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ">Draft<") {
		t.Errorf("Drafts folder row carries no Draft badge:\n%s", body)
	}
	adopt := `hx-get="/compose?adopt=` + strconv.FormatInt(msgs[0].ID, 10) + `" hx-target="#compose-root"`
	if !strings.Contains(body, adopt) {
		t.Errorf("Drafts folder row does not open compose (want %q):\n%s", adopt, body)
	}
	if strings.Contains(body, "hx-get=\"/t/") {
		t.Errorf("a draft row must not open the reading pane:\n%s", body)
	}
}
