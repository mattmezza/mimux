// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
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

// TestThreadSummaryContextFeedsWholeThread reproduces the bug this change
// fixes: a thread-level summarize used to be handed only the latest message's
// body. threadSummaryContext is what handleThreadSummary now feeds the AI
// client, so this pins that it carries every message in the conversation, not
// just the one the old single-message route would have used.
func TestThreadSummaryContextFeedsWholeThread(t *testing.T) {
	var inboxID, latestID int64
	s := serverWith(t, []config.Account{{Name: "A"}}, func(st *store.Store) {
		var err error
		inboxID, err = st.UpsertFolder("A", "INBOX", "inbox", 0)
		if err != nil {
			t.Fatal(err)
		}
		mk := func(uid uint32, mid, irt, refs, subj, snippet string, min int) {
			if err := st.UpsertMessage(&store.Message{
				Account: "A", FolderID: inboxID, UID: uid, MessageID: mid,
				InReplyTo: irt, Refs: refs, Subject: subj, FromName: mid, Snippet: snippet,
				Date: time.Date(2026, 1, 1, 0, min, 0, 0, time.UTC), IsRead: true,
			}); err != nil {
				t.Fatal(err)
			}
		}
		// A four-message thread: a (root) -> b -> c -> d (latest).
		mk(1, "a@x", "", "", "Deploy", "kickoff: let's ship Friday", 10)
		mk(2, "b@x", "a@x", "<a@x>", "Re: Deploy", "I can help with the rollout", 20)
		mk(3, "c@x", "b@x", "<a@x> <b@x>", "Re: Deploy", "found a blocker in staging", 30)
		mk(4, "d@x", "c@x", "<a@x> <b@x> <c@x>", "Re: Deploy", "blocker is fixed, ready to go", 40)
	})
	msgs, _ := s.store.ListMessages(inboxID, 10)
	for _, m := range msgs {
		if m.MessageID == "d@x" {
			latestID = m.ID
		}
	}
	if latestID == 0 {
		t.Fatal("seed: latest message d@x not found")
	}

	th := s.conversationOf(latestID)
	if th == nil || th.Count != 4 {
		t.Fatalf("conversationOf: got %+v, want a 4-message thread", th)
	}

	got := s.threadSummaryContext(context.Background(), th.Messages)
	for _, want := range []string{
		"kickoff: let's ship Friday",
		"I can help with the rollout",
		"found a blocker in staging",
		"blocker is fixed, ready to go",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("thread summary context is missing %q (single-message regression):\n%s", want, got)
		}
	}
	// Oldest-first: the kickoff message reads before the final "ready to go" one.
	if strings.Index(got, "kickoff") > strings.Index(got, "ready to go") {
		t.Errorf("thread summary context is not oldest-first:\n%s", got)
	}
}

// TestHandleThreadSummaryRoute checks the wiring end to end up to (but not
// including) the AI call: /t/{id}/summary resolves the whole conversation
// behind id, not just that one message. AIKey is left unset so aiClient's
// Enabled() is false — but the handler itself doesn't gate on that, so this
// only exercises routing, thread resolution and cache-key shape, not the
// provider call.
func TestHandleThreadSummaryRoute(t *testing.T) {
	var inboxID, latestID int64
	s := serverWith(t, []config.Account{{Name: "A"}}, func(st *store.Store) {
		var err error
		inboxID, err = st.UpsertFolder("A", "INBOX", "inbox", 0)
		if err != nil {
			t.Fatal(err)
		}
		mk := func(uid uint32, mid, irt, refs, subj, snippet string, min int) {
			if err := st.UpsertMessage(&store.Message{
				Account: "A", FolderID: inboxID, UID: uid, MessageID: mid,
				InReplyTo: irt, Refs: refs, Subject: subj, FromName: mid, Snippet: snippet,
				Date: time.Date(2026, 1, 1, 0, min, 0, 0, time.UTC), IsRead: true,
			}); err != nil {
				t.Fatal(err)
			}
		}
		mk(1, "a@x", "", "", "Deploy", "kickoff message", 10)
		mk(2, "b@x", "a@x", "<a@x>", "Re: Deploy", "final reply", 20)
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
	// Pre-seed the cache so the handler serves the cached view and never
	// reaches the AI client — the route/cache-key plumbing is what's under
	// test here, not the provider call.
	key := store.ThreadSummaryCacheKey(latestID, "brief")
	if err := s.store.SaveSummary(key, "- kickoff and final reply, both covered", false); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Get("/t/{id}/summary", s.handleThreadSummary)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t/"+strconv.FormatInt(latestID, 10)+"/summary?level=brief", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kickoff and final reply") {
		t.Errorf("did not serve the cached thread summary; body:\n%s", rec.Body.String())
	}

	// A per-message cache entry for the same message id must not leak into the
	// thread-scoped response: the two keys must not collide.
	msgKey := store.SummaryCacheKey(latestID, "brief")
	if err := s.store.SaveSummary(msgKey, "single-message-only summary", false); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t/"+strconv.FormatInt(latestID, 10)+"/summary?level=brief", nil))
	if strings.Contains(rec.Body.String(), "single-message-only summary") {
		t.Errorf("thread summary route served the per-message cache entry; body:\n%s", rec.Body.String())
	}
}
