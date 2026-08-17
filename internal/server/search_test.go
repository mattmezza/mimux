package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

func TestHighlightEscaping(t *testing.T) {
	// A subject carrying markup must be neutralized before any <mark> is added:
	// no XSS via the subject line.
	out := string(highlight(`<script>alert('x')</script>`, []string{"alert"}))
	if strings.Contains(out, "<script>") {
		t.Fatalf("raw <script> leaked: %q", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("markup not escaped: %q", out)
	}
	if !strings.Contains(out, `<mark`) || !strings.Contains(out, "alert") {
		t.Fatalf("term not highlighted: %q", out)
	}
}

func TestHighlightCaseInsensitive(t *testing.T) {
	out := string(highlight("Deploy the Deployment", []string{"deploy"}))
	if strings.Count(out, "<mark") != 2 {
		t.Fatalf("expected 2 marks, got %q", out)
	}
	// Original casing preserved inside the mark.
	if !strings.Contains(out, ">Deploy</mark>") {
		t.Fatalf("casing not preserved: %q", out)
	}
}

func TestHighlightNoTerms(t *testing.T) {
	out := string(highlight("a & b < c", nil))
	if out != "a &amp; b &lt; c" {
		t.Fatalf("escaping without terms = %q", out)
	}
}

func TestSearchEmptyState(t *testing.T) {
	s := serverWith(t, []config.Account{{Name: "A"}}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=test&scope=all", nil)
	s.handleSearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No messages match") {
		t.Fatalf("empty state missing; body:\n%s", rec.Body.String())
	}
}

func TestSearchHitHighlighted(t *testing.T) {
	s := serverWith(t, []config.Account{{Name: "A"}}, func(st *store.Store) {
		f, _ := st.UpsertFolder("A", "INBOX", "inbox", 0)
		_ = st.UpsertMessage(&store.Message{
			Account: "A", FolderID: f, UID: 1, MessageID: "m1",
			FromName: "Alice", FromAddress: "alice@x.com",
			Subject: "Quarterly report", Snippet: "numbers inside",
			Date: time.Now(), IsRead: true,
		})
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=quarterly&scope=all", nil)
	s.handleSearch(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(body, "<mark") || !strings.Contains(body, "Quarterly") {
		t.Fatalf("hit not highlighted; body:\n%s", body)
	}
}

// The htmx path returns the bare list partial and each result row carries an
// hx-push-url (so opening a result creates a history entry — Back returns to
// results); a plain navigation returns the full page instead.
func TestSearchHtmxPartialAndPushURL(t *testing.T) {
	s := serverWith(t, []config.Account{{Name: "A"}}, func(st *store.Store) {
		f, _ := st.UpsertFolder("A", "INBOX", "inbox", 0)
		_ = st.UpsertMessage(&store.Message{
			Account: "A", FolderID: f, UID: 1, MessageID: "m1",
			FromName: "Alice", FromAddress: "alice@x.com",
			Subject: "Quarterly report", Snippet: "numbers inside",
			Date: time.Now(), IsRead: true,
		})
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=quarterly&scope=all", nil)
	req.Header.Set("HX-Request", "true")
	s.handleSearch(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "<!DOCTYPE") {
		t.Fatalf("htmx request should return a partial, got full page:\n%s", body)
	}
	// & is HTML-escaped to &amp; inside the attribute; browsers decode it back.
	if !strings.Contains(body, `hx-push-url="/search?scope=all&amp;folder=0&amp;q=quarterly&m=1"`) {
		t.Fatalf("result row missing hx-push-url; body:\n%s", body)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/search?q=quarterly&scope=all", nil)
	s.handleSearch(rec, req) // no HX-Request header → full page
	if !strings.Contains(rec.Body.String(), "<!DOCTYPE") {
		t.Fatalf("plain navigation should return the full page")
	}
}
