// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestMessageHeadersModalUX(t *testing.T) {
	s := serverWith(t, nil, nil)
	w := httptest.NewRecorder()
	s.renderPartial(w, "message_headers", map[string]any{"Raw": "Received: one\r\nSubject: hello\r\n\r\n"})
	body := w.Body.String()
	for _, want := range []string{`role="dialog"`, `aria-modal="true"`, `data-message-headers`, `copyMessageHeaders()`, `closeSourceModal()`, "Received: one"} {
		if !strings.Contains(body, want) {
			t.Errorf("modal missing %q\n%s", want, body)
		}
	}
}

func TestSourceQuickActionsRender(t *testing.T) {
	s := serverWith(t, nil, nil)
	for _, id := range []string{"headers", "download-eml", "forward-eml"} {
		w := httptest.NewRecorder()
		s.renderPartial(w, "qa_source", map[string]any{"ID": id, "M": map[string]any{"ID": int64(9)}, "Btn": "button", "Icon": "icon", "Menu": true})
		if strings.TrimSpace(w.Body.String()) == "" {
			t.Errorf("%s rendered empty", id)
		}
	}
}

func TestForwardEMLOpenIsNonMutating(t *testing.T) {
	s, id := replyServer(t, serverWith(t, nil, nil).store.GetPrefs())
	r := httptest.NewRequest(http.MethodGet, "/compose?reply="+strconv.FormatInt(id, 10)+"&mode=forward-eml", nil)
	w := httptest.NewRecorder()
	s.handleComposeNew(w, r)
	if drafts, err := s.store.ListDrafts(); err != nil || len(drafts) != 0 {
		t.Fatalf("opening forward-eml created drafts: len=%d err=%v", len(drafts), err)
	}
	body := w.Body.String()
	for _, want := range []string{`name="forward_eml_id"`, `data-pending-forward-eml`, `Attached on save or send`} {
		if !strings.Contains(body, want) {
			t.Errorf("transient forward compose missing %q", want)
		}
	}
}

func TestSourceModalManagesKeyboardFocus(t *testing.T) {
	b, err := os.ReadFile("../../web/static/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	app := string(b)
	for _, want := range []string{"sourceModalReturnFocus", `e.key !== "Tab"`, `sourceModalReturnFocus.focus()`, `data-source-modal] button`} {
		if !strings.Contains(app, want) {
			t.Errorf("source modal focus lifecycle missing %q", want)
		}
	}
}
