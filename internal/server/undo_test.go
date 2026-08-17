// SPDX-License-Identifier: AGPL-3.0-only
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

func undoRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Post("/messages/{id}/archive", s.handleMove("archive"))
	r.Post("/messages/{id}/undo-move", s.handleUndoMove)
	return r
}

// TestArchiveThenUndo verifies the optimistic-move + undo flow: archiving
// relocates the local row immediately, and undoing within the grace period
// puts it back and cancels the deferred real IMAP move.
func TestArchiveThenUndo(t *testing.T) {
	var inboxID, archiveID, msgID int64
	s := serverWith(t, []config.Account{{Name: "A"}}, func(st *store.Store) {
		inboxID, _ = st.UpsertFolder("A", "INBOX", "inbox", 0)
		archiveID, _ = st.UpsertFolder("A", "Archive", "archive", 1)
		_ = st.UpsertMessage(&store.Message{Account: "A", FolderID: inboxID, UID: 1, MessageID: "m@x", Subject: "Hi"})
	})
	msgs, _ := s.store.ListMessages(inboxID, 10)
	if len(msgs) != 1 {
		t.Fatalf("seed: got %d messages", len(msgs))
	}
	msgID = msgs[0].ID
	r := undoRouter(s)

	// Archive: row moves to the Archive folder right away.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/messages/"+strconv.FormatInt(msgID, 10)+"/archive", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("archive status = %d", rec.Code)
	}
	m, _ := s.store.MessageByID(msgID)
	if m.FolderID != archiveID {
		t.Fatalf("after archive, FolderID = %d, want %d", m.FolderID, archiveID)
	}

	// Undo: back in the inbox, and a second undo has nothing left to cancel.
	rec = httptest.NewRecorder()
	form := url.Values{"folder": {strconv.FormatInt(inboxID, 10)}}
	req := httptest.NewRequest(http.MethodPost, "/messages/"+strconv.FormatInt(msgID, 10)+"/undo-move", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo status = %d, body: %s", rec.Code, rec.Body.String())
	}
	m, _ = s.store.MessageByID(msgID)
	if m.FolderID != inboxID {
		t.Fatalf("after undo, FolderID = %d, want %d", m.FolderID, inboxID)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/messages/"+strconv.FormatInt(msgID, 10)+"/undo-move", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second undo status = %d, want 409", rec.Code)
	}
}
