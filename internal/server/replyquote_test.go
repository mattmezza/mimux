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

// replyServer seeds a two-message conversation and returns the server plus the
// id of the message to reply to. No IMAP is reachable in a test, so the quote
// falls back to the stored snippet — which is the same fallback a real reply
// takes when the account is offline, and it exercises the whole prefill path.
func replyServer(t *testing.T, prefs store.Prefs) (*Server, int64) {
	t.Helper()
	var replyTo int64
	s := serverWith(t, []config.Account{{Name: "Work", Email: "me@work.com"}}, func(st *store.Store) {
		f, err := st.UpsertFolder("Work", "INBOX", "inbox", 0)
		if err != nil {
			t.Fatal(err)
		}
		mk := func(uid uint32, msgID, irt, snippet string, min int) {
			if err := st.UpsertMessage(&store.Message{
				Account: "Work", FolderID: f, UID: uid, MessageID: msgID, InReplyTo: irt,
				Subject: "Lunch", FromName: "Alice", FromAddress: "alice@example.com",
				ToAddresses: "me@work.com", Snippet: snippet, IsRead: true,
				Date: time.Date(2026, 7, 20, 10, min, 0, 0, time.UTC),
			}); err != nil {
				t.Fatal(err)
			}
		}
		mk(1, "first@example.com", "", "EARLIER-IN-THREAD", 10)
		mk(2, "second@example.com", "first@example.com", "one\ntwo\nthree\nfour", 30)
		// UpsertMessage doesn't hand back the row id; read it off the folder.
		msgs, err := st.ListMessages(f, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.MessageID == "second@example.com" {
				replyTo = m.ID
			}
		}
	})
	if err := s.store.SavePrefs(prefs); err != nil {
		t.Fatal(err)
	}
	return s, replyTo
}

func openReply(t *testing.T, s *Server, id int64) string {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/compose", s.handleComposeNew)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/compose?reply="+strconv.FormatInt(id, 10), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/compose?reply status = %d", rec.Code)
	}
	return rec.Body.String()
}

// TestReplyPrefillQuotesPerSetting: the reply window opens with the original
// quoted the way Settings → Composing says, in the compose mode it opens in.
func TestReplyPrefillQuotesPerSetting(t *testing.T) {
	cases := []struct {
		name           string
		mode, quote    string
		lines          int
		want, wantNone []string
	}{
		{
			name: "plain quotes the whole message", mode: "plain", quote: "all", lines: 10,
			want: []string{"wrote:", "&gt; one", "&gt; four"},
		},
		{
			name: "markdown blockquotes it", mode: "markdown", quote: "all", lines: 10,
			want: []string{"wrote:", "&gt; one", "&gt; four"},
		},
		{
			name: "html blockquotes it", mode: "html", quote: "all", lines: 10,
			want: []string{"<blockquote>", "wrote:"},
		},
		{
			name: "first N lines stops at N", mode: "plain", quote: "lines", lines: 2,
			want: []string{"&gt; one", "&gt; two", "[…]"}, wantNone: []string{"&gt; four"},
		},
		{
			name: "none quotes nothing", mode: "plain", quote: "none", lines: 10,
			wantNone: []string{"wrote:", "&gt; one"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, id := replyServer(t, store.Prefs{ComposeMode: c.mode, ReplyQuote: c.quote, ReplyQuoteLines: c.lines})
			body := openReply(t, s, id)
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("compose body is missing %q", w)
				}
			}
			for _, w := range c.wantNone {
				if strings.Contains(body, w) {
					t.Errorf("compose body should not contain %q", w)
				}
			}
		})
	}
}

// TestReplyAlwaysGivesTheAssistantTheConversation: "quote nothing" is about
// what the recipient reads back, not about what the assistant is shown — and
// the earlier messages of the thread ride along either way.
func TestReplyAlwaysGivesTheAssistantTheConversation(t *testing.T) {
	for _, quote := range []string{"all", "none"} {
		s, id := replyServer(t, store.Prefs{ComposeMode: "plain", ReplyQuote: quote, ReplyQuoteLines: 10})
		body := openReply(t, s, id)
		if !strings.Contains(body, "The message I am replying to") {
			t.Errorf("quote=%s: assistant context is missing the original", quote)
		}
		if !strings.Contains(body, "EARLIER-IN-THREAD") {
			t.Errorf("quote=%s: assistant context is missing the thread", quote)
		}
	}
}

// A forward carries the whole original whatever the reply-quote setting says.
func TestForwardIgnoresTheReplyQuoteSetting(t *testing.T) {
	s, id := replyServer(t, store.Prefs{ComposeMode: "plain", ReplyQuote: "none", ReplyQuoteLines: 10})
	r := chi.NewRouter()
	r.Get("/compose", s.handleComposeNew)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/compose?mode=forward&reply="+strconv.FormatInt(id, 10), nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Forwarded message") || !strings.Contains(body, "four") {
		t.Errorf("forward dropped the original:\n%s", body)
	}
}
