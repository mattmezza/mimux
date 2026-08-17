package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

func moveRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Post("/messages/{id}/move", s.handleMoveToFolder)
	return r
}

func postMove(t *testing.T, r http.Handler, msgID, folderID int64) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"folder": {strconv.FormatInt(folderID, 10)}}
	req := httptest.NewRequest(http.MethodPost, "/messages/"+strconv.FormatInt(msgID, 10)+"/move", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestMoveToFolder covers the handler's core logic: a same-account move
// relocates the local row, while a folder in a DIFFERENT account is rejected
// and leaves the message where it was.
func TestMoveToFolder(t *testing.T) {
	var inboxA, archiveA, inboxB, msgID int64
	s := serverWith(t, []config.Account{{Name: "A"}, {Name: "B"}}, func(st *store.Store) {
		inboxA, _ = st.UpsertFolder("A", "INBOX", "inbox", 0)
		archiveA, _ = st.UpsertFolder("A", "Archive", "archive", 1)
		inboxB, _ = st.UpsertFolder("B", "INBOX", "inbox", 0)
		_ = st.UpsertMessage(&store.Message{Account: "A", FolderID: inboxA, UID: 1, MessageID: "m@x", Subject: "Hi"})
	})
	msgs, _ := s.store.ListMessages(inboxA, 10)
	if len(msgs) != 1 {
		t.Fatalf("seed: got %d messages", len(msgs))
	}
	msgID = msgs[0].ID
	r := moveRouter(s)

	// Cross-account move is rejected; the message stays in account A's inbox.
	rec := postMove(t, r, msgID, inboxB)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-account move status = %d, want 400", rec.Code)
	}
	if m, _ := s.store.MessageByID(msgID); m.FolderID != inboxA {
		t.Fatalf("after rejected move, FolderID = %d, want %d", m.FolderID, inboxA)
	}

	// Same-account move relocates the row immediately.
	rec = postMove(t, r, msgID, archiveA)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-account move status = %d, want 200", rec.Code)
	}
	if m, _ := s.store.MessageByID(msgID); m.FolderID != archiveA {
		t.Fatalf("after move, FolderID = %d, want %d", m.FolderID, archiveA)
	}
}
