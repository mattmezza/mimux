package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

func reproRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/u", s.handleUnified)
	r.Get("/t/{id}", s.handleThread)
	r.Post("/messages/{id}/delete", s.handleMove("trash"))
	return r
}

func TestReproDeleteLatestFromThread(t *testing.T) {
	var inboxID int64
	s := serverWith(t, []config.Account{{Name: "A"}}, func(st *store.Store) {
		inboxID, _ = st.UpsertFolder("A", "INBOX", "inbox", 0)
		_, _ = st.UpsertFolder("A", "Trash", "trash", 5)
		mk := func(uid uint32, id, irt, refs string) {
			_ = st.UpsertMessage(&store.Message{
				Account: "A", FolderID: inboxID, UID: uid, MessageID: id,
				InReplyTo: irt, Refs: refs, Subject: "Re: Deploy", FromName: id, IsRead: true,
			})
		}
		mk(1, "a@x", "", "")
		mk(2, "b@x", "a@x", "<a@x>")
		mk(3, "c@x", "b@x", "<a@x> <b@x>")
	})
	msgs, _ := s.store.ListMessages(inboxID, 10)
	var cID int64
	for _, m := range msgs {
		if m.MessageID == "c@x" {
			cID = m.ID
		}
	}
	r := reproRouter(s)

	// Delete the LATEST message (c@x) of the thread.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/messages/"+strconv.FormatInt(cID, 10)+"/delete", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}

	// Request the thread by the now-deleted root id (as a stale unified row
	// would). It must NOT render the deleted message (whose folder/UID no longer
	// resolve — the body fetch would fail with "Could not load this message.");
	// it should drop to the empty reading pane instead.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t/"+strconv.FormatInt(cID, 10)+"?src=u", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("thread status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `id="message-detail"`) {
		t.Errorf("stale thread row rendered the deleted message; body:\n%s", body)
	}
	if !strings.Contains(body, "reading-pane-empty") {
		t.Errorf("stale thread row did not render the empty reading pane; body:\n%s", body)
	}
}
