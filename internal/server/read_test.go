package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
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
		req.Header.Set("X-Mimux-Thread", "1")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestMarkReadSurvivesSyncWhenIMAPPushFails pins the regression behind "I marked
// it read, then a re-sync showed it unread again": the handler writes is_read
// locally and pushes \Seen to IMAP in the background. When that push doesn't land
// (it errors, it hits submit's timeout while the worker reconnects, or the
// process restarts first — here: no account worker at all, so mail.SetRead fails)
// the local write must outrank the server's stale flags until the push is
// retried, not be quietly overwritten by the next sync.
func TestMarkReadSurvivesSyncWhenIMAPPushFails(t *testing.T) {
	var inbox int64
	s := serverWith(t, []config.Account{{Name: "A"}}, func(st *store.Store) {
		inbox, _ = st.UpsertFolder("A", "INBOX", "inbox", 0)
		_ = st.UpsertMessage(&store.Message{
			Account: "A", FolderID: inbox, UID: 7, MessageID: "solo@x",
			Subject: "Deploy", FromName: "solo@x",
			Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), IsRead: false,
		})
	})
	msgs, _ := s.store.ListMessages(inbox, 10)
	if len(msgs) != 1 {
		t.Fatalf("seed: got %d messages, want 1", len(msgs))
	}
	id := msgs[0].ID

	if rec := postMarkRead(t, readRouter(s), id, "read", false); rec.Code != http.StatusOK {
		t.Fatalf("mark read status = %d", rec.Code)
	}
	if m, _ := s.store.MessageByID(id); m == nil || !m.IsRead {
		t.Fatalf("mark read: message not read locally")
	}

	// Sync pulls the server's flags, which still say unread because the \Seen
	// push never landed. Both write paths must leave the pending row alone.
	_ = s.store.SetReadFromServer(id, false)
	if m, _ := s.store.MessageByID(id); m == nil || !m.IsRead {
		t.Errorf("flag sync reverted a mark-read whose \\Seen push is still owed")
	}
	_ = s.store.UpsertMessage(&store.Message{
		Account: "A", FolderID: inbox, UID: 7, MessageID: "solo@x",
		Subject: "Deploy", FromName: "solo@x",
		Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), IsRead: false,
	})
	if m, _ := s.store.MessageByID(id); m == nil || !m.IsRead {
		t.Errorf("re-fetch reverted a mark-read whose \\Seen push is still owed")
	}

	// Once the retry lands the flag, the server is authoritative again — so a
	// genuine "marked unread elsewhere" still propagates.
	_ = s.store.ClearSeenDirty(id, true)
	_ = s.store.SetReadFromServer(id, false)
	if m, _ := s.store.MessageByID(id); m == nil || m.IsRead {
		t.Errorf("after the push landed, the server's unread must win")
	}
}

// TestThreadReadUnread verifies the thread-level read/unread endpoint marks
// EVERY message in the thread (not just the row's latest), while a request
// without the X-Mimux-Thread header keeps single-message (per-message toggle)
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
