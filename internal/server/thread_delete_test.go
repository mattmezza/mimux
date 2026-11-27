// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

// threadDeleteRouter wires the routes the delete-then-reopen reproduction needs:
// POST /messages/{id}/delete (trash) and GET /t/{id} (thread detail).
func threadDeleteRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Post("/messages/{id}/delete", s.handleMove("trash"))
	r.Post("/t/{id}/delete", s.handleThreadMove("trash"))
	r.Post("/messages/undo-moves", s.handleUndoMoves)
	r.Get("/t/{id}", s.handleThread)
	return r
}

func TestDeleteConversationFromInboxWithSentReply(t *testing.T) {
	s := serverWith(t, nil, nil)
	addFolder := func(name, special string) int64 {
		t.Helper()
		id, err := s.store.UpsertFolder("A", name, special, 0)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	inbox := addFolder("INBOX", "inbox")
	sent := addFolder("Sent", "sent")
	trash := addFolder("Trash", "trash")
	addMessage := func(folder int64, id, reply string, min int) int64 {
		t.Helper()
		if err := s.store.UpsertMessage(&store.Message{
			Account: "A", FolderID: folder, UID: 1, MessageID: id, InReplyTo: reply,
			Subject: "Deploy", Date: time.Date(2026, 1, 1, 0, min, 0, 0, time.UTC), IsRead: true,
		}); err != nil {
			t.Fatal(err)
		}
		msg, err := s.store.MessageByFolderUID(folder, 1)
		if err != nil || msg == nil {
			t.Fatalf("message in folder %d: %v, %v", folder, msg, err)
		}
		return msg.ID
	}
	original := addMessage(inbox, "original@example.test", "", 10)
	reply := addMessage(sent, "reply@example.test", "original@example.test", 20)
	unrelated := addMessage(trash, "unrelated@example.test", "", 5)
	r := threadDeleteRouter(s)
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res := httptest.NewRecorder()
		r.ServeHTTP(res, req)
		return res
	}
	assertFolder := func(id, visible, imap int64) {
		t.Helper()
		msg, err := s.store.MessageByID(id)
		if err != nil || msg == nil || msg.FolderID != visible || msg.IMAPFolderID() != imap {
			t.Fatalf("message %d: got %+v (%v), want visible folder %d, IMAP folder %d", id, msg, err, visible, imap)
		}
	}

	seedID := strconv.FormatInt(original, 10)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t/"+seedID+"?src=u", nil))
	if !strings.Contains(rec.Body.String(), `moveThread('`+seedID+`','delete'`) {
		t.Fatal("thread pane Delete should target the original list row's ID")
	}
	rec = post("/t/"+seedID+"/delete", nil)
	var result threadMoveResult
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.IDs) != 2 || result.UndoToken == "" {
		t.Fatalf("delete thread: %d %s", rec.Code, rec.Body.String())
	}
	assertFolder(original, trash, inbox)
	assertFolder(reply, trash, sent)
	assertFolder(unrelated, trash, trash)
	if msgs, err := s.store.ListUnifiedInbox(10); err != nil || len(msgs) != 0 {
		t.Fatalf("conversation remains in inbox: %+v (%v)", msgs, err)
	}

	first := s.pending[original]
	rec = post("/messages/"+seedID+"/delete", nil)
	if rec.Code != http.StatusNoContent || first == nil || s.pending[original] != first {
		t.Fatalf("repeat delete changed the queued move: %d", rec.Code)
	}
	rec = post("/messages/undo-moves", url.Values{"id": result.IDs, "undo_token": {result.UndoToken}})
	if rec.Code != http.StatusOK {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body.String())
	}
	assertFolder(original, inbox, inbox)
	assertFolder(reply, sent, sent)
	assertFolder(unrelated, trash, trash)
	if rec := post("/messages/"+strconv.FormatInt(unrelated, 10)+"/delete", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete already in Trash: %d", rec.Code)
	}
	if len(s.pending) != 0 {
		t.Fatalf("unexpected pending moves: %v", s.pending)
	}
}

// TestThreadDetailAfterDeletingLatestMessage reproduces the bug where deleting
// a message that is the latest (root) of a thread leaves a stale thread row
// that, when clicked, re-fetches /t/{root}?src={folder}. The root message's
// row survives (moved to trash) but its folder/UID no longer resolve, so the
// old code fell through to handleMessage and rendered a message whose body
// fetch fails with "Could not load this message." The handler must instead drop
// the stale row to the empty reading pane.
func TestThreadDetailAfterDeletingLatestMessage(t *testing.T) {
	var inboxID, latestID int64
	s := serverWith(t, []config.Account{{Name: "A"}}, func(st *store.Store) {
		var err error
		inboxID, err = st.UpsertFolder("A", "INBOX", "inbox", 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = st.UpsertFolder("A", "Trash", "trash", 5); err != nil {
			t.Fatal(err)
		}
		mk := func(uid uint32, mid, irt, refs, subj string, min int) {
			if err := st.UpsertMessage(&store.Message{
				Account: "A", FolderID: inboxID, UID: uid, MessageID: mid,
				InReplyTo: irt, Refs: refs, Subject: subj, FromName: mid,
				Date: time.Date(2026, 1, 1, 0, min, 0, 0, time.UTC), IsRead: true,
			}); err != nil {
				t.Fatal(err)
			}
		}
		// Thread: a@x (root) -> b@x (reply, latest).
		mk(1, "a@x", "", "", "Deploy", 10)
		mk(2, "b@x", "a@x", "<a@x>", "Re: Deploy", 20)
	})
	msgs, _ := s.store.ListMessages(inboxID, 10)
	for _, m := range msgs {
		if m.MessageID == "b@x" {
			latestID = m.ID
		}
	}
	if latestID == 0 {
		t.Fatal("seed: latest message b@x not found")
	}
	r := threadDeleteRouter(s)

	// Sanity: the thread detail renders before the delete.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t/"+strconv.FormatInt(latestID, 10)+"?src="+strconv.FormatInt(inboxID, 10), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-delete /t status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="message-detail"`) {
		t.Fatalf("pre-delete /t did not render the thread; body:\n%s", rec.Body.String())
	}

	// Delete the latest message via the real handler (moves its row to trash).
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/messages/"+strconv.FormatInt(latestID, 10)+"/delete", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if m, _ := s.store.MessageByID(latestID); m == nil || m.FolderID == inboxID {
		t.Fatalf("delete did not move the message to trash")
	}

	// The stale thread row (msg-{latest}) is clicked again from the FOLDER view:
	// it must NOT render the deleted message (whose body would fail to load); it
	// should show the empty reading pane instead.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t/"+strconv.FormatInt(latestID, 10)+"?src="+strconv.FormatInt(inboxID, 10), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-delete /t status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `id="message-detail"`) {
		t.Errorf("stale thread row (folder scope) rendered the deleted message; body:\n%s", body)
	}
	if !strings.Contains(body, "reading-pane-empty") {
		t.Errorf("stale thread row (folder scope) did not render the empty reading pane; body:\n%s", body)
	}

	// Same from the UNIFIED view (src=u): the deleted message is no longer in an
	// inbox folder, so the stale row must also drop to the empty pane.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t/"+strconv.FormatInt(latestID, 10)+"?src=u", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-delete /t (unified) status = %d", rec.Code)
	}
	body = rec.Body.String()
	if strings.Contains(body, `id="message-detail"`) {
		t.Errorf("stale thread row (unified) rendered the deleted message; body:\n%s", body)
	}
	if !strings.Contains(body, "reading-pane-empty") {
		t.Errorf("stale thread row (unified) did not render the empty reading pane; body:\n%s", body)
	}
}
