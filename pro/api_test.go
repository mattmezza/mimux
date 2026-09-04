//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/ext"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// testAPI wires the real extension router — the same auth middleware, scope
// groups and handlers the pro binary mounts — over a seeded store and a
// Manager with no live IMAP workers (accounts exist in config, but any
// operation that needs a connection fails, which is exactly what the
// offline-tolerant paths are tested against).
type testAPI struct {
	h      http.Handler
	st     *store.Store
	secret string // token with every scope
}

func newTestAPI(t *testing.T) *testAPI { return newTestAPIWith(t, nil, nil) }

// newTestAPIWith is newTestAPI with a say in the bootstrap config and a chance
// to seed the store before the router (and with it the licence gate) is built.
func newTestAPIWith(t *testing.T, cfg *config.Config, seed func(*store.Store)) *testAPI {
	t.Helper()
	st := openStore(t)
	if seed != nil {
		seed(st)
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Accounts = []config.Account{{Name: "a1", Email: "me@example.test"}}
	m := mail.NewManager(cfg, st)
	h := routes(ext.Deps{Mail: m, Store: st, Cfg: cfg}).Handler
	secret := mintToken(t, st, &store.APIToken{
		Label: "all", Scopes: "mail:read mail:send mail:modify accounts:read webhooks:manage",
	})
	return &testAPI{h: h, st: st, secret: secret}
}

// req performs an authenticated API request; body may be nil or a JSON-able.
func (ta *testAPI) req(t *testing.T, method, path string, body any, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Header.Set("Authorization", "Bearer "+ta.secret)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	ta.h.ServeHTTP(rec, r)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("bad JSON response: %v\n%s", err, rec.Body.String())
	}
}

// seedFolder creates a folder and returns its id.
func seedFolder(t *testing.T, st *store.Store, account, name, special string) int64 {
	t.Helper()
	id, err := st.UpsertFolder(account, name, special, 0)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// seedMsg inserts a message and returns its assigned id.
func seedMsg(t *testing.T, st *store.Store, m store.Message) int64 {
	t.Helper()
	if m.Date.IsZero() {
		m.Date = time.Now().UTC()
	}
	if err := st.UpsertMessage(&m); err != nil {
		t.Fatal(err)
	}
	got, err := st.MessageByFolderUID(m.FolderID, m.UID)
	if err != nil || got == nil {
		t.Fatalf("seeded message not found: %v", err)
	}
	return got.ID
}

func TestAccountsEndpoint(t *testing.T) {
	ta := newTestAPI(t)
	rec := ta.req(t, http.MethodGet, "/v1/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("accounts = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"data"`
	}
	decodeBody(t, rec, &out)
	if len(out.Data) != 1 || out.Data[0].Name != "a1" || out.Data[0].Email != "me@example.test" {
		t.Fatalf("unexpected accounts: %s", rec.Body.String())
	}
	// The response must be a field-picked view, never a marshalled
	// config.Account — that struct carries credentials.
	for _, forbidden := range []string{"Password", "password", "OAuth2ClientSecret", "client_secret"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("accounts response leaks %q: %s", forbidden, rec.Body.String())
		}
	}
}

func TestFoldersTree(t *testing.T) {
	ta := newTestAPI(t)
	seedFolder(t, ta.st, "a1", "INBOX", "inbox")
	seedFolder(t, ta.st, "a1", "Work/Clients", "")
	seedFolder(t, ta.st, "a1", "Work/Clients/Acme", "")

	rec := ta.req(t, http.MethodGet, "/v1/folders?account=a1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("folders = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []struct {
			Account string `json:"account"`
			Folders []struct {
				Label      string `json:"label"`
				SpecialUse string `json:"special_use"`
				Children   []struct {
					Label    string `json:"label"`
					ID       int64  `json:"id"`
					Children []struct {
						Label string `json:"label"`
					} `json:"children"`
				} `json:"children"`
			} `json:"folders"`
		} `json:"data"`
	}
	decodeBody(t, rec, &out)
	if len(out.Data) != 1 || len(out.Data[0].Folders) != 2 {
		t.Fatalf("want inbox + Work roots: %s", rec.Body.String())
	}
	work := out.Data[0].Folders[1]
	if work.Label != "Work" || len(work.Children) != 1 || work.Children[0].Label != "Clients" ||
		len(work.Children[0].Children) != 1 || work.Children[0].Children[0].Label != "Acme" {
		t.Fatalf("bad tree: %s", rec.Body.String())
	}
}

// TestListMessagesPagesByMessage: the API's limit counts MESSAGES, not
// conversations — `mimux mail list -unread -limit 1` handing back a whole
// five-message thread was the bug — while the cursor still enumerates the scope
// exactly once and thread_size still reports the whole conversation.
func TestListMessagesPagesByMessage(t *testing.T) {
	ta := newTestAPI(t)
	inbox := seedFolder(t, ta.st, "a1", "INBOX", "inbox")
	base := time.Now().UTC().Add(-24 * time.Hour)
	// The newest five messages are one conversation; three singles sit below it.
	for i := range 5 {
		seedMsg(t, ta.st, store.Message{
			Account: "a1", FolderID: inbox, UID: uint32(i + 1),
			MessageID: fmt.Sprintf("t%d@x", i), Refs: "t0@x", Subject: "thread",
			Date: base.Add(time.Duration(10+i) * time.Minute),
		})
	}
	for i := range 3 {
		seedMsg(t, ta.st, store.Message{
			Account: "a1", FolderID: inbox, UID: uint32(100 + i),
			MessageID: fmt.Sprintf("s%d@x", i), Subject: "single",
			Date: base.Add(time.Duration(i) * time.Minute),
		})
	}

	var out struct {
		Data       []messageJSON `json:"data"`
		NextCursor string        `json:"next_cursor"`
	}
	seen := map[int64]bool{}
	cursor, pages := "", 0
	for ; pages < 20; pages++ {
		url := "/v1/messages?unread=true&limit=1&folder=" + fmt.Sprint(inbox)
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := ta.req(t, http.MethodGet, url, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
		}
		decodeBody(t, rec, &out)
		if len(out.Data) > 1 {
			t.Fatalf("limit=1 returned %d messages: %s", len(out.Data), rec.Body.String())
		}
		for _, m := range out.Data {
			if seen[m.ID] {
				t.Fatalf("message %d returned twice across pages", m.ID)
			}
			seen[m.ID] = true
			if m.Subject == "thread" && m.ThreadSize != 5 {
				t.Errorf("thread_size = %d on a one-message page, want the whole conversation's 5", m.ThreadSize)
			}
		}
		cursor = out.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 8 {
		t.Fatalf("walking limit=1 pages found %d of 8 messages (%d pages)", len(seen), pages)
	}
}

func TestListMessagesFiltersAndPagination(t *testing.T) {
	ta := newTestAPI(t)
	inbox := seedFolder(t, ta.st, "a1", "INBOX", "inbox")
	base := time.Now().UTC().Add(-24 * time.Hour)
	for i := range 25 {
		seedMsg(t, ta.st, store.Message{
			Account: "a1", FolderID: inbox, UID: uint32(i + 1),
			MessageID: fmt.Sprintf("<m%d@x>", i), Subject: fmt.Sprintf("msg %d", i),
			FromName: "Alice", FromAddress: "alice@example.test", ToAddresses: "me@example.test",
			Date: base.Add(time.Duration(i) * time.Minute), IsRead: i%2 == 0, IsStarred: i == 3,
		})
	}

	// Filter: unread only.
	rec := ta.req(t, http.MethodGet, "/v1/messages?folder="+fmt.Sprint(inbox)+"&unread=true", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data       []messageJSON `json:"data"`
		NextCursor string        `json:"next_cursor"`
	}
	decodeBody(t, rec, &out)
	if len(out.Data) != 12 {
		t.Fatalf("unread filter: want 12, got %d", len(out.Data))
	}
	for _, m := range out.Data {
		if m.IsRead {
			t.Fatalf("unread filter returned a read message: %+v", m)
		}
	}

	// Filter: starred + from.
	rec = ta.req(t, http.MethodGet, "/v1/messages?folder="+fmt.Sprint(inbox)+"&starred=true&from=alice", nil)
	decodeBody(t, rec, &out)
	if len(out.Data) != 1 || !out.Data[0].IsStarred {
		t.Fatalf("starred filter: %s", rec.Body.String())
	}

	// Pagination: walk the folder in pages of 10 until the cursor runs out.
	seen := map[int64]bool{}
	cursor := ""
	for range 10 {
		url := "/v1/messages?folder=" + fmt.Sprint(inbox) + "&limit=10"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec = ta.req(t, http.MethodGet, url, nil)
		decodeBody(t, rec, &out)
		for _, m := range out.Data {
			if seen[m.ID] {
				t.Fatalf("message %d returned twice across pages", m.ID)
			}
			seen[m.ID] = true
		}
		cursor = out.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 25 {
		t.Fatalf("pagination walked %d of 25 messages", len(seen))
	}

	// Bad limit is rejected.
	rec = ta.req(t, http.MethodGet, "/v1/messages?limit=9999", nil)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_request" {
		t.Fatalf("limit 9999 = %d", rec.Code)
	}
	// Unknown folder 404s.
	rec = ta.req(t, http.MethodGet, "/v1/messages?folder=424242", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown folder = %d", rec.Code)
	}
}

func TestGetMessage(t *testing.T) {
	ta := newTestAPI(t)
	inbox := seedFolder(t, ta.st, "a1", "INBOX", "inbox")
	id := seedMsg(t, ta.st, store.Message{
		Account: "a1", FolderID: inbox, UID: 1, MessageID: "<one@x>",
		Subject: "hello", FromAddress: "alice@example.test", ToAddresses: "me@example.test, bob@example.test",
	})

	rec := ta.req(t, http.MethodGet, fmt.Sprintf("/v1/messages/%d", id), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		messageJSON
		Body      *struct{ Text, HTML string } `json:"body"`
		BodyError string                       `json:"body_error"`
	}
	decodeBody(t, rec, &out)
	if out.Subject != "hello" || len(out.To) != 2 {
		t.Fatalf("bad metadata: %s", rec.Body.String())
	}
	// No live IMAP in tests: the body can't be fetched, and that must not fail
	// the request — metadata with body_error is the contract.
	if out.BodyError == "" || out.Body != nil {
		t.Fatalf("want body_error without live IMAP, got: %s", rec.Body.String())
	}

	if rec := ta.req(t, http.MethodGet, fmt.Sprintf("/v1/messages/%d?body=nope", id), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body param = %d", rec.Code)
	}
	if rec := ta.req(t, http.MethodGet, "/v1/messages/999999", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing message = %d", rec.Code)
	}
}

func TestPatchMessage(t *testing.T) {
	ta := newTestAPI(t)
	inbox := seedFolder(t, ta.st, "a1", "INBOX", "inbox")
	other := seedFolder(t, ta.st, "a1", "Receipts", "")
	foreign := seedFolder(t, ta.st, "a2", "INBOX", "inbox")
	id := seedMsg(t, ta.st, store.Message{Account: "a1", FolderID: inbox, UID: 1, MessageID: "<p@x>", Subject: "patch me"})

	rec := ta.req(t, http.MethodPatch, fmt.Sprintf("/v1/messages/%d", id),
		map[string]any{"read": true, "starred": true, "labels_add": []string{"todo"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	var out messageJSON
	decodeBody(t, rec, &out)
	if !out.IsRead || !out.IsStarred || len(out.Labels) != 1 || out.Labels[0] != "todo" {
		t.Fatalf("patch didn't stick: %+v", out)
	}

	// Label removal (fresh decode target: omitted fields don't overwrite).
	rec = ta.req(t, http.MethodPatch, fmt.Sprintf("/v1/messages/%d", id), map[string]any{"labels_remove": []string{"todo"}})
	var afterRemove messageJSON
	decodeBody(t, rec, &afterRemove)
	if len(afterRemove.Labels) != 0 {
		t.Fatalf("label not removed: %+v", afterRemove.Labels)
	}

	// Move within the account.
	rec = ta.req(t, http.MethodPatch, fmt.Sprintf("/v1/messages/%d", id), map[string]any{"folder_id": other})
	decodeBody(t, rec, &out)
	if out.FolderID != other {
		t.Fatalf("move: folder = %d, want %d", out.FolderID, other)
	}

	// Cross-account move is rejected.
	rec = ta.req(t, http.MethodPatch, fmt.Sprintf("/v1/messages/%d", id), map[string]any{"folder_id": foreign})
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_request" {
		t.Fatalf("cross-account move = %d: %s", rec.Code, rec.Body.String())
	}

	// Unknown JSON fields fail loudly.
	rec = ta.req(t, http.MethodPatch, fmt.Sprintf("/v1/messages/%d", id), map[string]any{"maek_red": true})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("typo'd field = %d, want 400", rec.Code)
	}
}

func TestArchiveSpamDelete(t *testing.T) {
	ta := newTestAPI(t)
	inbox := seedFolder(t, ta.st, "a1", "INBOX", "inbox")
	archive := seedFolder(t, ta.st, "a1", "Archive", "archive")
	trash := seedFolder(t, ta.st, "a1", "Trash", "trash")

	arch := seedMsg(t, ta.st, store.Message{Account: "a1", FolderID: inbox, UID: 1, MessageID: "<a@x>"})
	del := seedMsg(t, ta.st, store.Message{Account: "a1", FolderID: inbox, UID: 2, MessageID: "<d@x>"})
	spam := seedMsg(t, ta.st, store.Message{Account: "a1", FolderID: inbox, UID: 3, MessageID: "<s@x>"})

	if rec := ta.req(t, http.MethodPost, fmt.Sprintf("/v1/messages/%d/archive", arch), nil); rec.Code != http.StatusOK {
		t.Fatalf("archive = %d: %s", rec.Code, rec.Body.String())
	}
	if m, _ := ta.st.MessageByID(arch); m.FolderID != archive {
		t.Fatalf("archived message in folder %d, want %d", m.FolderID, archive)
	}

	if rec := ta.req(t, http.MethodDelete, fmt.Sprintf("/v1/messages/%d", del), nil); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	if m, _ := ta.st.MessageByID(del); m.FolderID != trash {
		t.Fatalf("deleted message in folder %d, want trash %d", m.FolderID, trash)
	}

	// No spam folder exists: clean 400, message stays put.
	rec := ta.req(t, http.MethodPost, fmt.Sprintf("/v1/messages/%d/spam", spam), nil)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_request" {
		t.Fatalf("spam without folder = %d: %s", rec.Code, rec.Body.String())
	}
	if m, _ := ta.st.MessageByID(spam); m.FolderID != inbox {
		t.Fatalf("spam-move without target moved the message anyway")
	}
}

func TestSendScheduledAndIdempotent(t *testing.T) {
	ta := newTestAPI(t)
	inbox := seedFolder(t, ta.st, "a1", "INBOX", "inbox")
	orig := seedMsg(t, ta.st, store.Message{
		Account: "a1", FolderID: inbox, UID: 1,
		MessageID: "<orig@x>", Refs: "<root@x>", Subject: "original",
	})

	sendAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	body := map[string]any{
		"to": []string{"bob@example.test"}, "subject": "Re: original", "body": "hi",
		"in_reply_to": orig, "schedule_at": sendAt.Format(time.RFC3339),
	}
	rec := ta.req(t, http.MethodPost, "/v1/messages/send", body, "Idempotency-Key", "k1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("scheduled send = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Status   string `json:"status"`
		OutboxID int64  `json:"outbox_id"`
	}
	decodeBody(t, rec, &out)
	if out.Status != "scheduled" || out.OutboxID == 0 {
		t.Fatalf("bad schedule response: %s", rec.Body.String())
	}
	o, err := ta.st.OutboxByID(out.OutboxID)
	if err != nil || o == nil {
		t.Fatal("outbox row missing")
	}
	if o.Account != "a1" || o.InReplyTo != "<orig@x>" || !strings.Contains(o.References, "<orig@x>") {
		t.Fatalf("threading headers not derived: %+v", o)
	}
	if !o.SendAt.Equal(sendAt) {
		t.Fatalf("send_at = %v, want %v", o.SendAt, sendAt)
	}

	// Same Idempotency-Key: replayed, not re-queued.
	rec2 := ta.req(t, http.MethodPost, "/v1/messages/send", body, "Idempotency-Key", "k1")
	if rec2.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("second call not replayed: %d %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.String() != rec.Body.String() {
		t.Fatalf("replay body differs: %s vs %s", rec2.Body.String(), rec.Body.String())
	}
	if rows, _ := ta.st.ListScheduled(); len(rows) != 1 {
		t.Fatalf("idempotent retry queued %d messages, want 1", len(rows))
	}

	// Validation.
	if rec := ta.req(t, http.MethodPost, "/v1/messages/send", map[string]any{"subject": "x"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("no recipients = %d", rec.Code)
	}
	if rec := ta.req(t, http.MethodPost, "/v1/messages/send",
		map[string]any{"to": []string{"b@x"}, "account": "nope"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown account = %d", rec.Code)
	}
	if rec := ta.req(t, http.MethodPost, "/v1/messages/send",
		map[string]any{"to": []string{"b@x"}, "in_reply_to": 999999}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown in_reply_to = %d", rec.Code)
	}

	// Immediate send without live SMTP fails upstream, with the stable code.
	rec = ta.req(t, http.MethodPost, "/v1/messages/send", map[string]any{"to": []string{"b@x"}})
	if rec.Code != http.StatusBadGateway || errCode(t, rec) != "send_failed" {
		t.Fatalf("offline send = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDrafts(t *testing.T) {
	ta := newTestAPI(t)
	inbox := seedFolder(t, ta.st, "a1", "INBOX", "inbox")
	orig := seedMsg(t, ta.st, store.Message{Account: "a1", FolderID: inbox, UID: 1, MessageID: "<orig@x>"})

	rec := ta.req(t, http.MethodPost, "/v1/drafts", map[string]any{
		"to": "bob@example.test", "subject": "wip", "body": "…", "in_reply_to": orig,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create draft = %d: %s", rec.Code, rec.Body.String())
	}
	var d draftJSON
	decodeBody(t, rec, &d)
	if d.ID == 0 || d.Account != "a1" {
		t.Fatalf("draft not created properly: %+v", d)
	}
	stored, _ := ta.st.DraftByID(d.ID)
	if stored.InReplyTo != "<orig@x>" {
		t.Fatalf("in_reply_to not resolved to Message-ID: %q", stored.InReplyTo)
	}

	// Patch merges only the provided fields.
	rec = ta.req(t, http.MethodPatch, fmt.Sprintf("/v1/drafts/%d", d.ID), map[string]any{"subject": "done"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch draft = %d: %s", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &d)
	if d.Subject != "done" || d.To != "bob@example.test" {
		t.Fatalf("patch merged wrong: %+v", d)
	}

	if rec := ta.req(t, http.MethodPatch, "/v1/drafts/999", map[string]any{"subject": "x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("patch missing draft = %d", rec.Code)
	}
}

func TestFiltersAPI(t *testing.T) {
	ta := newTestAPI(t)
	rule := map[string]any{
		"name": "newsletters", "enabled": true,
		"conditions": []map[string]any{{"field": "from", "op": "contains", "value": "news@"}},
		"actions":    []map[string]any{{"type": "mark_read"}},
	}
	rec := ta.req(t, http.MethodPost, "/v1/filters", rule)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create filter = %d: %s", rec.Code, rec.Body.String())
	}

	bad := map[string]any{
		"name": "broken", "conditions": []map[string]any{{"field": "from", "op": "regexp?", "value": "["}},
		"actions": []map[string]any{{"type": "mark_read"}},
	}
	if rec := ta.req(t, http.MethodPost, "/v1/filters", bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid filter = %d: %s", rec.Code, rec.Body.String())
	}

	rec = ta.req(t, http.MethodGet, "/v1/filters", nil)
	var out struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	decodeBody(t, rec, &out)
	if len(out.Data) != 1 || out.Data[0].Name != "newsletters" {
		t.Fatalf("list filters: %s", rec.Body.String())
	}
}

func TestLocalSearch(t *testing.T) {
	ta := newTestAPI(t)
	inbox := seedFolder(t, ta.st, "a1", "INBOX", "inbox")
	seedMsg(t, ta.st, store.Message{Account: "a1", FolderID: inbox, UID: 1, MessageID: "<h@x>", Subject: "hello world", FromAddress: "alice@example.test"})
	seedMsg(t, ta.st, store.Message{Account: "a1", FolderID: inbox, UID: 2, MessageID: "<g@x>", Subject: "goodbye", FromAddress: "bob@example.test"})

	rec := ta.req(t, http.MethodPost, "/v1/messages/search", map[string]any{"query": "subject:hello"})
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []messageJSON `json:"data"`
	}
	decodeBody(t, rec, &out)
	if len(out.Data) != 1 || out.Data[0].Subject != "hello world" {
		t.Fatalf("search results: %s", rec.Body.String())
	}

	if rec := ta.req(t, http.MethodPost, "/v1/messages/search", map[string]any{"query": "  "}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty query = %d", rec.Code)
	}
	if rec := ta.req(t, http.MethodPost, "/v1/messages/search", map[string]any{"query": "x", "mode": "psychic"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode = %d", rec.Code)
	}
}

func TestDeepSearchJob(t *testing.T) {
	ta := newTestAPI(t)
	seedFolder(t, ta.st, "a1", "INBOX", "inbox")

	rec := ta.req(t, http.MethodPost, "/v1/messages/search", map[string]any{"query": "from:alice", "mode": "deep"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deep search = %d: %s", rec.Code, rec.Body.String())
	}
	var job struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	decodeBody(t, rec, &job)
	if job.ID == "" {
		t.Fatalf("no job id: %s", rec.Body.String())
	}

	// Without live IMAP every folder search fails and the job completes empty.
	deadline := time.Now().Add(15 * time.Second)
	for {
		rec = ta.req(t, http.MethodGet, "/v1/search/jobs/"+job.ID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("job poll = %d: %s", rec.Code, rec.Body.String())
		}
		var poll struct {
			Status  string        `json:"status"`
			Results []messageJSON `json:"results"`
		}
		decodeBody(t, rec, &poll)
		if poll.Status == "done" {
			if len(poll.Results) != 0 {
				t.Fatalf("expected empty results offline, got %d", len(poll.Results))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deep search job never completed")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if rec := ta.req(t, http.MethodGet, "/v1/search/jobs/doesnotexist", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job = %d", rec.Code)
	}
}

func TestScopesPerRouteGroup(t *testing.T) {
	ta := newTestAPI(t)
	readOnly := mintToken(t, ta.st, &store.APIToken{Label: "ro", Scopes: "mail:read"})

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/accounts"},                // accounts:read
		{http.MethodPost, "/v1/messages/send"},          // mail:send
		{http.MethodPost, "/v1/messages/1/forward-eml"}, // mail:send
		{http.MethodPatch, "/v1/messages/1"},            // mail:modify
		{http.MethodPost, "/v1/filters"},                // mail:modify
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+readOnly)
		rec := httptest.NewRecorder()
		ta.h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden || errCode(t, rec) != "insufficient_scope" {
			t.Errorf("%s %s with mail:read = %d, want 403 insufficient_scope", c.method, c.path, rec.Code)
		}
	}
	// And mail:read still works where it should.
	r := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer "+readOnly)
	rec := httptest.NewRecorder()
	ta.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /v1/messages with mail:read = %d, want 200", rec.Code)
	}
}

func TestOpenAPISpecIsPublic(t *testing.T) {
	ta := newTestAPI(t)
	// No Authorization header: the spec must still come back.
	r := httptest.NewRequest(http.MethodGet, "/v1/openapi.json", nil)
	rec := httptest.NewRecorder()
	ta.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi.json = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var spec map[string]any
	decodeBody(t, rec, &spec)
	if spec["openapi"] == nil {
		t.Fatalf("spec has no openapi version key")
	}
	// The authenticated neighbours must not have been opened up by proximity.
	r = httptest.NewRequest(http.MethodGet, "/v1/tokens/self", nil)
	rec = httptest.NewRecorder()
	ta.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated tokens/self = %d, want 401", rec.Code)
	}
}
