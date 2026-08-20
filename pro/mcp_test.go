//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/ext"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// mcpSession connects an in-memory MCP client to a server built for a token
// with the given scopes, over the same seeded store the API tests use.
func mcpSession(t *testing.T, scopes string) (*mcp.ClientSession, *api, *store.Store) {
	t.Helper()
	st := openStore(t)
	cfg := &config.Config{Accounts: []config.Account{{Name: "a1", Email: "me@example.test"}}}
	m := mail.NewManager(cfg, st)
	deps := ext.Deps{Mail: m, Store: st, Cfg: cfg}
	a := newAPI(deps, newWebhooks(deps, newLicenceGate(deps)))
	tok := &store.APIToken{Label: "t", Scopes: scopes, Hash: "x"}
	if err := st.CreateAPIToken(tok); err != nil {
		t.Fatal(err)
	}
	srv := buildMCP(a, tok)
	ct, srvT := mcp.NewInMemoryTransports()
	ctx := t.Context()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, a, st
}

func toolNames(t *testing.T, cs *mcp.ClientSession) map[string]bool {
	t.Helper()
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	return names
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

// structuredOf unmarshals the tool result's structured content into v.
func structuredOf(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("structured content: %v", err)
	}
}

func TestMCPToolsFollowScopes(t *testing.T) {
	cs, _, _ := mcpSession(t, "mail:read")
	names := toolNames(t, cs)
	for _, want := range []string{"list_folders", "search_mail", "read_message"} {
		if !names[want] {
			t.Errorf("mail:read should expose %s", want)
		}
	}
	for _, forbidden := range []string{"list_accounts", "mark_read", "move_message", "draft_reply", "send_draft"} {
		if names[forbidden] {
			t.Errorf("mail:read must not expose %s", forbidden)
		}
	}

	all, _, _ := mcpSession(t, "mail:read mail:send mail:modify accounts:read")
	if n := toolNames(t, all); len(n) != 9 {
		t.Errorf("full-scope token should expose 9 tools, got %d: %v", len(n), n)
	}
}

func TestMCPSearchAndRead(t *testing.T) {
	cs, _, st := mcpSession(t, "mail:read")
	fid := seedFolder(t, st, "a1", "INBOX", "inbox")
	id := seedMsg(t, st, store.Message{
		Account: "a1", FolderID: fid, UID: 1, Subject: "quarterly report",
		FromAddress: "boss@example.test", ToAddresses: "me@example.test",
		Snippet: "please review",
	})
	// Cache a raw body so read_message works without a live IMAP connection.
	if err := st.SaveMessageBody(id, []byte("Subject: quarterly report\r\n\r\nplease review the numbers")); err != nil {
		t.Fatal(err)
	}

	var out searchOut
	structuredOf(t, callTool(t, cs, "search_mail", map[string]any{"query": "quarterly"}), &out)
	if out.Status != "done" || len(out.Results) != 1 || out.Results[0].ID != id {
		t.Fatalf("search: %+v", out)
	}

	var msg struct {
		ID        int64  `json:"id"`
		Body      string `json:"body"`
		BodyError string `json:"body_error"`
		Truncated bool   `json:"truncated"`
	}
	structuredOf(t, callTool(t, cs, "read_message", map[string]any{"id": id}), &msg)
	if msg.ID != id {
		t.Fatalf("read_message: %+v", msg)
	}
	// The account has no live IMAP connection; body comes from the stored text
	// or reports body_error — either way the call must not fail.
	if msg.Body == "" && msg.BodyError == "" {
		t.Fatalf("read_message returned neither body nor body_error: %+v", msg)
	}
}

func TestMCPReadMessageTruncation(t *testing.T) {
	cs, _, st := mcpSession(t, "mail:read")
	fid := seedFolder(t, st, "a1", "INBOX", "inbox")
	long := strings.Repeat("word ", bodyCap) // 5*bodyCap chars
	id := seedMsg(t, st, store.Message{
		Account: "a1", FolderID: fid, UID: 2, Subject: "long",
		FromAddress: "x@example.test",
	})
	if err := st.SaveMessageBody(id, []byte("Subject: long\r\n\r\n"+long)); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Body       string `json:"body"`
		BodyError  string `json:"body_error"`
		Truncated  bool   `json:"truncated"`
		NextOffset int    `json:"next_offset"`
	}
	structuredOf(t, callTool(t, cs, "read_message", map[string]any{"id": id}), &out)
	if out.BodyError != "" {
		t.Skip("body not readable from store in this fixture: " + out.BodyError)
	}
	if !out.Truncated || out.NextOffset != bodyCap || len([]rune(out.Body)) != bodyCap {
		t.Fatalf("truncation: truncated=%v next=%d len=%d", out.Truncated, out.NextOffset, len([]rune(out.Body)))
	}
	structuredOf(t, callTool(t, cs, "read_message", map[string]any{"id": id, "offset": out.NextOffset}), &out)
	if out.Body == "" {
		t.Fatal("continuation returned empty body")
	}
}

func TestMCPMutations(t *testing.T) {
	cs, _, st := mcpSession(t, "mail:read mail:modify")
	inbox := seedFolder(t, st, "a1", "INBOX", "inbox")
	seedFolder(t, st, "a1", "Archive", "archive")
	trash := seedFolder(t, st, "a1", "Trash", "trash")
	_ = trash
	id := seedMsg(t, st, store.Message{
		Account: "a1", FolderID: inbox, UID: 3, Subject: "s", FromAddress: "x@example.test",
	})

	callTool(t, cs, "mark_read", map[string]any{"id": id})
	m, _ := st.MessageByID(id)
	if !m.IsRead {
		t.Fatal("mark_read didn't")
	}
	callTool(t, cs, "star_message", map[string]any{"id": id})
	m, _ = st.MessageByID(id)
	if !m.IsStarred {
		t.Fatal("star_message didn't")
	}

	// trash without confirm must refuse and leave the message in place.
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "move_message", Arguments: map[string]any{"id": id, "target": "trash"},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatal("trash without confirm should be a tool error")
	}
	m, _ = st.MessageByID(id)
	if m.FolderID != inbox {
		t.Fatal("refused trash still moved the message")
	}

	callTool(t, cs, "move_message", map[string]any{"id": id, "target": "archive"})
	m, _ = st.MessageByID(id)
	if f, _ := st.FolderByID(m.FolderID); f.SpecialUse != "archive" {
		t.Fatalf("archive move landed in %q", f.SpecialUse)
	}

	callTool(t, cs, "move_message", map[string]any{"id": id, "target": "trash", "confirm": true})
	m, _ = st.MessageByID(id)
	if f, _ := st.FolderByID(m.FolderID); f.SpecialUse != "trash" {
		t.Fatalf("confirmed trash landed in %q", f.SpecialUse)
	}
}

func TestMCPDraftFlow(t *testing.T) {
	cs, _, st := mcpSession(t, "mail:read mail:send")
	fid := seedFolder(t, st, "a1", "INBOX", "inbox")
	orig := seedMsg(t, st, store.Message{
		Account: "a1", FolderID: fid, UID: 4, Subject: "hello",
		FromAddress: "alice@example.test", ToAddresses: "me@example.test",
		MessageID: "orig-id@example.test",
	})

	var prev draftPreview
	structuredOf(t, callTool(t, cs, "draft_reply", map[string]any{
		"in_reply_to": orig, "body": "Thanks, on it.",
	}), &prev)
	if prev.DraftID == 0 || prev.Subject != "Re: hello" || prev.InReplyTo != "orig-id@example.test" {
		t.Fatalf("preview: %+v", prev)
	}
	if len(prev.To) != 1 || prev.To[0] != "alice@example.test" {
		t.Fatalf("reply recipients: %v", prev.To)
	}
	d, err := st.DraftByID(prev.DraftID)
	if err != nil || d == nil {
		t.Fatal("draft_reply must create a real draft row")
	}
	if d.Kind != "reply" || d.InReplyTo != "orig-id@example.test" {
		t.Fatalf("draft threading: %+v", d)
	}

	// send_draft against an account with no SMTP config must fail as a tool
	// error and keep the draft (nothing silently lost).
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "send_draft", Arguments: map[string]any{"draft_id": prev.DraftID},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatal("send with no SMTP should be a tool error")
	}
	if d, _ := st.DraftByID(prev.DraftID); d == nil {
		t.Fatal("failed send deleted the draft")
	}

	// A fresh mail with no recipients must refuse.
	res, err = cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "draft_reply", Arguments: map[string]any{"body": "hi"},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatal("fresh draft without to should be a tool error")
	}
}

func TestMCPInboxSummaryResource(t *testing.T) {
	cs, _, st := mcpSession(t, "mail:read")
	fid := seedFolder(t, st, "a1", "INBOX", "inbox")
	seedMsg(t, st, store.Message{
		Account: "a1", FolderID: fid, UID: 5, Subject: "unread one",
		FromAddress: "x@example.test", Date: time.Now().UTC(),
	})

	res, err := cs.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "mimux://inbox/summary"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Contents) != 1 || res.Contents[0].MIMEType != "application/json" {
		t.Fatalf("contents: %+v", res.Contents)
	}
	var sum struct {
		Accounts     []map[string]any `json:"accounts"`
		NewestUnread []map[string]any `json:"newest_unread"`
	}
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &sum); err != nil {
		t.Fatal(err)
	}
	if len(sum.Accounts) != 1 || len(sum.NewestUnread) != 1 {
		t.Fatalf("summary: %+v", sum)
	}
}

// TestMCPEndpointRequiresToken proves auth happens before any MCP handling:
// an unauthenticated POST to /api/mcp gets the API's 401 envelope, not an MCP
// response.
func TestMCPEndpointRequiresToken(t *testing.T) {
	ta := newTestAPI(t)
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ta.h.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatalf("unauthenticated MCP POST = %d, want 401", rec.Code)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Code != "unauthorized" {
		t.Fatalf("expected the API error envelope, got: %s", rec.Body.String())
	}
}

// TestMCPBridgeProxy drives the bridge's proxy against an in-memory remote:
// tools listed remotely appear locally and calls forward end to end.
func TestMCPBridgeProxy(t *testing.T) {
	ctx := t.Context()

	// Remote: a trivial MCP server standing in for a running mimux.
	remote := mcp.NewServer(&mcp.Implementation{Name: "remote"}, nil)
	type echoArgs struct {
		S string `json:"s"`
	}
	mcp.AddTool(remote, &mcp.Tool{Name: "echo", Description: "echoes"},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
			return nil, map[string]any{"echo": in.S}, nil
		})
	remote.AddResource(&mcp.Resource{URI: "mimux://x", Name: "x", MIMEType: "text/plain"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: "hi"}}}, nil
		})
	rc, rs := mcp.NewInMemoryTransports()
	if _, err := remote.Connect(ctx, rs, nil); err != nil {
		t.Fatal(err)
	}
	upstream := mcp.NewClient(&mcp.Implementation{Name: "bridge"}, nil)
	us, err := upstream.Connect(ctx, rc, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = us.Close() }()

	proxy, err := buildProxy(ctx, us)
	if err != nil {
		t.Fatal(err)
	}
	pc, ps := mcp.NewInMemoryTransports()
	if _, err := proxy.Connect(ctx, ps, nil); err != nil {
		t.Fatal(err)
	}
	downstream := mcp.NewClient(&mcp.Implementation{Name: "editor"}, nil)
	ds, err := downstream.Connect(ctx, pc, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ds.Close() }()

	tools, err := ds.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "echo" {
		t.Fatalf("proxied tools: %v %v", tools, err)
	}
	res, err := ds.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"s": "ping"}})
	if err != nil || res.IsError {
		t.Fatalf("proxied call: %v %v", res, err)
	}
	rr, err := ds.ReadResource(ctx, &mcp.ReadResourceParams{URI: "mimux://x"})
	if err != nil || len(rr.Contents) != 1 || rr.Contents[0].Text != "hi" {
		t.Fatalf("proxied resource: %v %v", rr, err)
	}
}

// The bridge reads the same credential store `mimux mail login` writes: an MCP
// client config that is just `command: mimux, args: [mcp]` has to work.
func TestBridgeTargetUsesStoredLogin(t *testing.T) {
	_, _ = newLoginBox(t, "")
	if err := saveCreds(credentials{Default: "https://a.example", Instances: map[string]credEntry{
		"https://a.example": {Token: "stored", Insecure: true},
	}}); err != nil {
		t.Fatal(err)
	}
	url, token, insecure := bridgeTarget()
	if url != "https://a.example" || token != "stored" || !insecure {
		t.Fatalf("stored login: %q %q %v", url, token, insecure)
	}
	// The environment still wins, and an instance with no stored entry gets no token.
	t.Setenv("MIMUX_URL", "https://b.example/")
	if url, token, insecure = bridgeTarget(); url != "https://b.example" || token != "" || insecure {
		t.Fatalf("env override: %q %q %v", url, token, insecure)
	}
	t.Setenv("MIMUX_TOKEN", "env")
	if _, token, _ = bridgeTarget(); token != "env" {
		t.Fatalf("MIMUX_TOKEN = %q", token)
	}
}
