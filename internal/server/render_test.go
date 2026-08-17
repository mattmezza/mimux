// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// serverWith builds a Server over a fresh store seeded by seed.
func serverWith(t *testing.T, accounts []config.Account, seed func(*store.Store)) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if seed != nil {
		seed(st)
	}
	cfg := &config.Config{Server: config.Server{BaseURL: "http://localhost"}, Accounts: accounts}
	srv, err := New(cfg, st, mail.NewManager(cfg, st), "test")
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func listRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/u", s.handleUnified)
	r.Get("/f/{id}", s.handleFolder)
	r.Get("/t/{id}", s.handleThread)
	return r
}

// TestRenderThreadedUnifiedList seeds a 3-message conversation split across two
// accounts and verifies the unified list renders a thread row with a count
// badge, and the thread detail renders every message without a template error.
func TestRenderThreadedViews(t *testing.T) {
	var latestID int64
	accounts := []config.Account{{Name: "A"}, {Name: "B"}}
	s := serverWith(t, accounts, func(st *store.Store) {
		fa, _ := st.UpsertFolder("A", "INBOX", "inbox", 0)
		fb, _ := st.UpsertFolder("B", "INBOX", "inbox", 0)
		mk := func(acc string, folder int64, uid uint32, id, irt, refs, subj string, min int) {
			_ = st.UpsertMessage(&store.Message{
				Account: acc, FolderID: folder, UID: uid, MessageID: id,
				InReplyTo: irt, Refs: refs, Subject: subj, FromName: id,
				Date: time.Date(2026, 1, 1, 0, min, 0, 0, time.UTC), IsRead: true,
			})
		}
		mk("A", fa, 1, "a@x", "", "", "Deploy", 10)
		mk("B", fb, 1, "b@x", "a@x", "<a@x>", "Re: Deploy", 20)
		mk("A", fa, 2, "c@x", "b@x", "<a@x> <b@x>", "Re: Deploy", 30)
	})
	// Latest message id is c@x (uid 2 in folder A).
	msgs, _ := s.store.ListUnifiedInbox(100)
	for _, m := range msgs {
		if m.MessageID == "c@x" {
			latestID = m.ID
		}
	}

	// Unified list must contain one thread row with a "3" count badge.
	rec := httptest.NewRecorder()
	listRouter(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/u", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/u status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `title="3 messages"`) {
		t.Errorf("/u missing thread count badge; body:\n%s", body)
	}

	// Thread detail must render all three senders.
	rec = httptest.NewRecorder()
	listRouter(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t/"+strconv.FormatInt(latestID, 10)+"?src=u", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/t status = %d", rec.Code)
	}
	det := rec.Body.String()
	for _, want := range []string{"a@x", "b@x", "c@x", "3 messages"} {
		if !strings.Contains(det, want) {
			t.Errorf("thread detail missing %q", want)
		}
	}
}

// TestRenderUnifiedEmpty exercises the nil-Folder empty state (regression: the
// empty branch must not dereference a nil folder).
func TestRenderUnifiedEmpty(t *testing.T) {
	s := serverWith(t, []config.Account{{Name: "A"}, {Name: "B"}}, nil)
	rec := httptest.NewRecorder()
	listRouter(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/u", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "caught up") {
		t.Errorf("empty unified list did not render the caught-up state")
	}
}

// TestRenderHealthRows checks the drawer/sidebar health rows show a relative
// last-sync time per account, falling back to "OK" before the first sync.
func TestRenderHealthRows(t *testing.T) {
	s := serverWith(t, []config.Account{{Name: "A"}}, nil)
	statuses := []mail.AccountStatus{
		{Account: "synced", State: "ok", LastSync: time.Now().Add(-5 * time.Minute)},
		{Account: "fresh", State: "ok", LastSync: time.Now()},
		{Account: "never", State: "ok"},
		{Account: "broken", State: "error", Message: "boom"},
	}
	rec := httptest.NewRecorder()
	s.renderPartial(rec, "health_rows", statuses)
	body := rec.Body.String()
	for _, want := range []string{"5m ago", ">now<", ">OK<", ">error<"} {
		if !strings.Contains(body, want) {
			t.Errorf("health rows missing %q; body:\n%s", want, body)
		}
	}
	if strings.Contains(body, "now ago") {
		t.Errorf("a just-synced account must not read \"now ago\"; body:\n%s", body)
	}
}

// TestEventsOpensWithSyncState pins the SSE contract the spinner rides on: the
// stream states the aggregate sync flag the moment it opens, so a browser that
// just reconnected (holding a page rendered who-knows-when) is corrected
// without a second endpoint to ask.
func TestEventsOpensWithSyncState(t *testing.T) {
	s := serverWith(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // handler writes its preamble, then sees a dead client and returns
	w := httptest.NewRecorder()
	s.handleEvents(w, httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx))

	// No accounts running, so nothing is syncing.
	if got := w.Body.String(); !strings.Contains(got, "event: sync-status\ndata: 0\n\n") {
		t.Fatalf("stream preamble = %q, want a sync-status of 0 in it", got)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
}
