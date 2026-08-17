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

// threadDeleteRouter wires the routes the delete-then-reopen reproduction needs:
// POST /messages/{id}/delete (trash) and GET /t/{id} (thread detail).
func threadDeleteRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Post("/messages/{id}/delete", s.handleMove("trash"))
	r.Get("/t/{id}", s.handleThread)
	return r
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
