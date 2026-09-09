// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestFailedForwardEMLSaveKeepsUploadsAndSource(t *testing.T) {
	s := testServer(t)
	fields := url.Values{"draft_id": {"0"}, "account": {"Personal"}, "to": {"ada@example.com"}, "subject": {"Fwd: source"}, "body": {"keep my edits"}, "kind": {"forward"}, "mode": {"plain"}, "forward_eml_id": {"999999"}}
	rec := httptest.NewRecorder()
	s.handleComposeDraftSave(rec, composeUpload(t, "/compose/draft", fields, "notes.txt", "keep me"))
	drafts, err := s.store.ListDrafts()
	if err != nil || len(drafts) != 1 || drafts[0].ForwardEMLID != 999999 {
		t.Fatalf("pending EML draft lost: %+v, %v", drafts, err)
	}
	atts, err := s.store.DraftAttachments(drafts[0].ID)
	if err != nil || len(atts) != 1 || string(atts[0].Data) != "keep me" {
		t.Fatalf("fresh upload lost after EML failure: %+v, %v", atts, err)
	}
	rec = httptest.NewRecorder()
	s.handleComposeNew(rec, httptest.NewRequest("GET", "/compose?draft="+strconv.FormatInt(drafts[0].ID, 10), nil))
	if !strings.Contains(rec.Body.String(), `name="forward_eml_id" value="999999"`) {
		t.Fatal("reopen dropped pending EML source")
	}
}

func TestFailedForwardEMLSendKeepsUploadsAndSource(t *testing.T) {
	s := testServer(t)
	fields := url.Values{"draft_id": {"0"}, "from": {"me@gmail.com"}, "to": {"ada@example.com"}, "subject": {"Fwd: source"}, "body": {"keep my edits"}, "kind": {"forward"}, "mode": {"plain"}, "forward_eml_id": {"999999"}, "send_mode": {"later"}}
	rec := httptest.NewRecorder()
	s.handleComposeSend(rec, composeUpload(t, "/compose", fields, "notes.txt", "keep me"))
	drafts, err := s.store.ListDrafts()
	if err != nil || len(drafts) != 1 || drafts[0].ForwardEMLID != 999999 {
		t.Fatalf("failed send lost pending source: %+v, %v", drafts, err)
	}
	atts, err := s.store.DraftAttachments(drafts[0].ID)
	if err != nil || len(atts) != 1 || string(atts[0].Data) != "keep me" {
		t.Fatalf("fresh upload lost: %+v, %v", atts, err)
	}
}
