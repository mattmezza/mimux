package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/store"
)

// TestLabelAddRemove walks the whole add → render → remove path: the POST
// persists the label in the space-joined column and swaps back a pill row that
// links to the label: search with the stored token (spaces are underscores).
func TestLabelAddRemove(t *testing.T) {
	var id int64
	s := serverWith(t, []config.Account{{Name: "A"}}, func(st *store.Store) {
		f, _ := st.UpsertFolder("A", "INBOX", "inbox", 0)
		_ = st.UpsertMessage(&store.Message{Account: "A", FolderID: f, UID: 1,
			Subject: "hi", Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		m, _ := st.MessageByFolderUID(f, 1)
		id = m.ID
	})
	r := chi.NewRouter()
	r.Post("/messages/{id}/label", s.handleLabel)
	post := func(query string) string {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/messages/"+strconv.FormatInt(id, 10)+"/label"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s = %d", query, rec.Code)
		}
		return rec.Body.String()
	}

	body := post("?label=Project+X")
	if m, _ := s.store.MessageByID(id); m.Labels != "Project_X" {
		t.Fatalf("stored labels = %q, want %q", m.Labels, "Project_X")
	}
	for _, want := range []string{">Project X<", "q=label:Project_X", "labels-" + strconv.FormatInt(id, 10)} {
		if !strings.Contains(body, want) {
			t.Errorf("pill row missing %q; got:\n%s", want, body)
		}
	}

	// The row context menu offers the same add, with the label now in use
	// listed as a one-click item.
	r.Get("/rowmenu/{id}", s.handleRowMenu)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rowmenu/"+strconv.FormatInt(id, 10), nil))
	if menu := rec.Body.String(); !strings.Contains(menu, "Add label") || !strings.Contains(menu, "label?label=Project_X") {
		t.Errorf("row menu missing the add-label section; got:\n%s", menu)
	}

	post("?remove=1&label=Project+X") // removable by the displayed form
	if m, _ := s.store.MessageByID(id); m.Labels != "" {
		t.Fatalf("labels after remove = %q", m.Labels)
	}
}
