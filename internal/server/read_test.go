package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/store"
)

func readRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Post("/messages/{id}/read", s.handleMarkRead(true))
	r.Post("/messages/{id}/unread", s.handleMarkRead(false))
	return r
}

func postMarkRead(t *testing.T, r http.Handler, id int64, path string, thread bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/messages/"+strconv.FormatInt(id, 10)+"/"+path, nil)
	if thread {
		req.Header.Set("X-SM-Thread", "1")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestThreadReadUnread verifies the thread-level read/unread endpoint marks
// EVERY message in the thread (not just the row's latest), while a request
// without the X-SM-Thread header keeps single-message (per-message toggle)
// semantics.
func TestThreadReadUnread(t *testing.T) {
	var inbox int64
	var latestID, midID, oldestID int64
	s := serverWith(t, []config.Account{{Name: "A"}}, func(st *store.Store) {
		inbox, _ = st.UpsertFolder("A", "INBOX", "inbox", 0)
		mk := func(uid uint32, mid, irt, refs string, min int, read bool) {
			_ = st.UpsertMessage(&store.Message{
				Account: "A", FolderID: inbox, UID: uid, MessageID: mid,
				InReplyTo: irt, Refs: refs, Subject: "Deploy", FromName: mid,
				Date: time.Date(2026, 1, 1, 0, min, 0, 0, time.UTC), IsRead: read,
			})
		}
		mk(1, "a@x", "", "", 10, false)               // oldest, unread
		mk(2, "b@x", "a@x", "<a@x>", 20, false)       // mid, unread
		mk(3, "c@x", "b@x", "<a@x> <b@x>", 30, false) // latest, unread
	})
	msgs, _ := s.store.ListMessages(inbox, 100)
	for _, m := range msgs {
		switch m.MessageID {
		case "a@x":
			oldestID = m.ID
		case "b@x":
			midID = m.ID
		case "c@x":
			latestID = m.ID
		}
	}
	if oldestID == 0 || midID == 0 || latestID == 0 {
		t.Fatalf("seed: missing thread messages (a=%d b=%d c=%d)", oldestID, midID, latestID)
	}
	r := readRouter(s)

	// Thread-level mark read with only the latest message id must persist to
	// every message in the thread.
	if rec := postMarkRead(t, r, latestID, "read", true); rec.Code != http.StatusOK {
		t.Fatalf("thread read status = %d", rec.Code)
	}
	for id, name := range map[int64]string{oldestID: "a@x", midID: "b@x", latestID: "c@x"} {
		if m, _ := s.store.MessageByID(id); m == nil || !m.IsRead {
			t.Errorf("thread read: %s (%d) not marked read", name, id)
		}
	}

	// Thread-level mark unread flips the whole thread back.
	if rec := postMarkRead(t, r, latestID, "unread", true); rec.Code != http.StatusOK {
		t.Fatalf("thread unread status = %d", rec.Code)
	}
	for id, name := range map[int64]string{oldestID: "a@x", midID: "b@x", latestID: "c@x"} {
		if m, _ := s.store.MessageByID(id); m == nil || m.IsRead {
			t.Errorf("thread unread: %s (%d) should be unread", name, id)
		}
	}

	// Without the header it is a single-message op: marking a non-latest
	// message read touches only that message, not its siblings.
	if rec := postMarkRead(t, r, midID, "read", false); rec.Code != http.StatusOK {
		t.Fatalf("single read status = %d", rec.Code)
	}
	if m, _ := s.store.MessageByID(midID); m == nil || !m.IsRead {
		t.Errorf("single read: b@x (%d) not marked read", midID)
	}
	if m, _ := s.store.MessageByID(oldestID); m == nil || m.IsRead {
		t.Errorf("single read: sibling a@x (%d) should stay unread", oldestID)
	}
}
