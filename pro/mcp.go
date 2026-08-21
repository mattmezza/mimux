//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/search"
	"github.com/mattmezza/mimux/internal/store"
)

// The MCP server is the same product as the REST API wearing the protocol an
// agent already speaks: identical tokens, identical scopes, the same domain
// layer underneath. It mounts at /api/mcp behind tokenAuth.require, and each
// request builds a server exposing only the tools its token's scopes allow —
// so tools/list IS the permission model, and a missing tool tells the model
// the credential can't do that, not that mimux can't.
//
// Transport is stateless streamable HTTP: no session table, no affinity, every
// POST self-contained. The stdio bridge in mcp_bridge.go proxies for clients
// that only speak stdio.

// bodyCap is how much of a message body one read_message call returns. Long
// bodies page via `offset` — an explicit continuation, never a silent cut.
const bodyCap = 20000

// deepSearchWait is how long search_mail with deep=true waits for the IMAP
// fan-out before handing back the job id instead of results.
const deepSearchWait = 15 * time.Second

func newMCPHandler(a *api) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return buildMCP(a, tokenOf(r))
	}, &mcp.StreamableHTTPOptions{Stateless: true})
}

// buildMCP assembles the per-request server for one token. Cheap by design:
// nine closures over an already-constructed api.
func buildMCP(a *api, tok *store.APIToken) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "mimux", Title: "mimux mail"}, nil)

	if tok.HasScope("accounts:read") {
		mcp.AddTool(s, &mcp.Tool{
			Name: "list_accounts",
			Description: "List the configured mail accounts with live sync state (ok/syncing/error), " +
				"last sync time and message/unread counts. Use it to learn account names for other tools " +
				"and to check whether mail is currently syncing before trusting freshness.",
		}, a.mcpListAccounts)
	}

	if tok.HasScope("mail:read") {
		mcp.AddTool(s, &mcp.Tool{
			Name: "list_folders",
			Description: "List the folder tree per account (id, name, special role like inbox/sent/archive/spam/trash, " +
				"unread count). Folder ids are what move_message and search folder filters take. " +
				"Omit `account` to get every account's tree.",
		}, a.mcpListFolders)
		mcp.AddTool(s, &mcp.Tool{
			Name: "search_mail",
			Description: "Search mail and get structured results (id, account, folder_id, from, subject, date, snippet, flags). " +
				"The query language: bare words match anywhere; `from:`, `to:`, `subject:`, `body:` narrow fields; " +
				"`is:unread`, `is:starred`, `has:attachment`; `before:`/`after:` with YYYY-MM-DD; quoted phrases; `-` negates. " +
				"Default searches the local index — fast and usually enough. Set deep=true only when the local index " +
				"may not have it (very old mail, rarely-synced folders): that asks the IMAP servers directly and can " +
				"take ~15s; if it returns a job_id with status running, poll by calling this tool again with the same " +
				"query — a finished job answers instantly from cache.",
		}, a.mcpSearch)
		mcp.AddTool(s, &mcp.Tool{
			Name: "read_message",
			Description: "Read one message by id: sender, recipients, subject, date, flags, labels, attachment list " +
				"and the body as plain text (format=html returns sanitised HTML instead). A long body is returned " +
				"in chunks: when `truncated` is true, call again with `offset` set to `next_offset` for the next " +
				"chunk — never assume a truncated body is complete. Set `headers` only when the raw message headers " +
				"themselves are the question (deliverability, routing, a Received: chain): they are long, and left " +
				"out otherwise.",
		}, a.mcpReadMessage)
	}

	if tok.HasScope("mail:modify") {
		mcp.AddTool(s, &mcp.Tool{
			Name:        "mark_read",
			Description: "Mark a message read (default) or unread (read=false). Returns the updated flags.",
		}, a.mcpMarkRead)
		mcp.AddTool(s, &mcp.Tool{
			Name: "star_message",
			Description: "Star a message (default) or unstar it (starred=false). Starring is the polite way to flag " +
				"something for the human's attention without moving it.",
		}, a.mcpStar)
		mcp.AddTool(s, &mcp.Tool{
			Name: "move_message",
			Description: "Move a message to a folder id (from list_folders) or a special target: archive, spam or trash. " +
				"Moving to trash deletes mail from the human's point of view, so it additionally requires confirm=true — " +
				"ask the human before setting it. Archive is the safe default for cleaning up an inbox.",
		}, a.mcpMove)
	}

	if tok.HasScope("mail:send") {
		mcp.AddTool(s, &mcp.Tool{
			Name: "draft_reply",
			Description: "Create a draft — a reply when in_reply_to is a message id (recipients, subject and threading " +
				"are derived from the original; reply_all=true includes everyone), or a fresh mail when it is omitted " +
				"(then `to` is required). NOTHING IS SENT: the draft sits in mimux's Drafts until send_draft is called. " +
				"Returns the draft id and a full preview of exactly what would go out — show that preview to the human " +
				"and get their approval before calling send_draft.",
		}, a.mcpDraftReply)
		mcp.AddTool(s, &mcp.Tool{
			Name: "send_draft",
			Description: "Send a previously created draft by id, exactly as previewed, then delete the draft. " +
				"This is the only tool that sends mail. Only call it after the human has seen the draft_reply " +
				"preview and approved it.",
		}, a.mcpSendDraft)
	}

	if tok.HasScope("mail:read") {
		s.AddResource(&mcp.Resource{
			URI:         "mimux://inbox/summary",
			Name:        "inbox-summary",
			Title:       "Unified inbox summary",
			Description: "Per-account sync state and unread counts, plus the newest unread messages across all accounts.",
			MIMEType:    "application/json",
		}, a.mcpInboxSummary)
	}
	return s
}

// structured wraps a value as a CallToolResult carrying both structured
// content and its JSON as text, so clients that read only one of the two see
// the same thing.
func structured(v any) (*mcp.CallToolResult, any, error) {
	return nil, v, nil
}

// --- read tools ---

type emptyArgs struct{}

func (a *api) mcpListAccounts(_ context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
	view, err := a.accountsView()
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't read account stats: %w", err)
	}
	return structured(map[string]any{"accounts": view})
}

type listFoldersArgs struct {
	Account string `json:"account,omitempty" jsonschema:"account name; omit for all accounts"`
}

func (a *api) mcpListFolders(_ context.Context, _ *mcp.CallToolRequest, in listFoldersArgs) (*mcp.CallToolResult, any, error) {
	names := a.accountNames(in.Account)
	if len(names) == 0 {
		return nil, nil, fmt.Errorf("no account named %q", in.Account)
	}
	view, err := a.foldersView(names)
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't list folders: %w", err)
	}
	return structured(map[string]any{"accounts": view})
}

type searchArgs struct {
	Query    string `json:"query" jsonschema:"the search query, in the syntax from the tool description"`
	Account  string `json:"account,omitempty" jsonschema:"restrict to one account"`
	FolderID int64  `json:"folder_id,omitempty" jsonschema:"restrict to one folder id"`
	Deep     bool   `json:"deep,omitempty" jsonschema:"ask the IMAP servers directly instead of the local index"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max results, default 100"`
}

type searchOut struct {
	Status  string        `json:"status"` // done | running
	JobID   string        `json:"job_id,omitempty"`
	Results []messageJSON `json:"results,omitempty"`
}

func (a *api) mcpSearch(ctx context.Context, _ *mcp.CallToolRequest, in searchArgs) (*mcp.CallToolResult, any, error) {
	q := search.Parse(in.Query)
	if q.Empty() {
		return nil, nil, fmt.Errorf("query is empty")
	}
	scope := searchScopeFor(in.Account, in.FolderID)
	limit := clampLimit(in.Limit)

	if !in.Deep {
		out, err := a.localSearch(q, scope, in.Account, in.FolderID, limit)
		if err != nil {
			return nil, nil, fmt.Errorf("search failed: %w", err)
		}
		return structured(searchOut{Status: "done", Results: out})
	}

	job := a.startDeepSearch(in.Query, q, scope, in.Account, in.FolderID)
	deadline := time.NewTimer(deepSearchWait)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if ids, done := job.snapshot(); done {
			msgs, _ := a.store.MessagesByIDs(ids)
			out := make([]messageJSON, 0, len(msgs))
			for _, m := range msgs {
				out = append(out, toMessageJSON(m))
			}
			if len(out) > limit {
				out = out[:limit]
			}
			return structured(searchOut{Status: "done", Results: out})
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-deadline.C:
			return structured(searchOut{Status: "running", JobID: job.ID})
		case <-tick.C:
		}
	}
}

type readMessageArgs struct {
	ID      int64  `json:"id" jsonschema:"the message id"`
	Format  string `json:"format,omitempty" jsonschema:"text (default) or html"`
	Offset  int    `json:"offset,omitempty" jsonschema:"body continuation offset from a previous truncated read"`
	Headers string `json:"headers,omitempty" jsonschema:"raw, parsed or both to include the message headers; omitted by default"`
}

func (a *api) mcpReadMessage(ctx context.Context, _ *mcp.CallToolRequest, in readMessageArgs) (*mcp.CallToolResult, any, error) {
	msg, err := a.store.MessageByID(in.ID)
	if err != nil || msg == nil {
		return nil, nil, fmt.Errorf("no message with id %d", in.ID)
	}
	var body string
	switch in.Format {
	case "", "text":
		body, err = a.mail.PlainText(ctx, msg)
	case "html":
		body, _, err = a.mail.Body(ctx, msg, true, false)
	default:
		return nil, nil, fmt.Errorf("format must be text or html")
	}
	if in.Headers != "" && in.Headers != "raw" && in.Headers != "parsed" && in.Headers != "both" {
		return nil, nil, fmt.Errorf("headers must be raw, parsed or both")
	}
	out := struct {
		messageJSON
		Body          string              `json:"body"`
		BodyError     string              `json:"body_error,omitempty"`
		Truncated     bool                `json:"truncated,omitempty"`
		NextOffset    int                 `json:"next_offset,omitempty"`
		HeadersRaw    string              `json:"headers_raw,omitempty"`
		HeadersParsed map[string][]string `json:"headers_parsed,omitempty"`
		HeadersError  string              `json:"headers_error,omitempty"`
		Attachments   []map[string]any    `json:"attachments,omitempty"`
	}{messageJSON: toMessageJSON(*msg)}
	if err != nil {
		out.BodyError = "Couldn't fetch the body — the account may be offline."
	} else {
		runes := []rune(body)
		if in.Offset < 0 || in.Offset > len(runes) {
			return nil, nil, fmt.Errorf("offset %d is beyond the body (%d)", in.Offset, len(runes))
		}
		end := in.Offset + bodyCap
		if end >= len(runes) {
			end = len(runes)
		} else {
			out.Truncated, out.NextOffset = true, end
		}
		out.Body = string(runes[in.Offset:end])
	}
	if in.Headers != "" {
		raw, parsed, herr := a.mail.Headers(ctx, msg)
		switch {
		case herr != nil:
			out.HeadersError = "Couldn't fetch the headers — the account may be offline."
		case in.Headers == "parsed":
			out.HeadersParsed = parsed
		case in.Headers == "raw":
			out.HeadersRaw = raw
		default:
			out.HeadersRaw, out.HeadersParsed = raw, parsed
		}
	}
	if msg.HasAttachment {
		if atts, aerr := a.mail.Attachments(ctx, msg); aerr == nil {
			for i, at := range atts {
				out.Attachments = append(out.Attachments, map[string]any{
					"index": i, "filename": at.Filename, "media_type": at.MediaType, "size": at.Size,
				})
			}
		}
	}
	return structured(out)
}

// --- modify tools ---

type flagArgs struct {
	ID      int64 `json:"id" jsonschema:"the message id"`
	Read    *bool `json:"read,omitempty" jsonschema:"mark_read only: false to mark unread; default true"`
	Starred *bool `json:"starred,omitempty" jsonschema:"star_message only: false to unstar; default true"`
}

func (a *api) mcpMarkRead(_ context.Context, _ *mcp.CallToolRequest, in flagArgs) (*mcp.CallToolResult, any, error) {
	msg, err := a.store.MessageByID(in.ID)
	if err != nil || msg == nil {
		return nil, nil, fmt.Errorf("no message with id %d", in.ID)
	}
	read := in.Read == nil || *in.Read
	a.applyRead(msg, read)
	return structured(map[string]any{"id": msg.ID, "is_read": read})
}

func (a *api) mcpStar(_ context.Context, _ *mcp.CallToolRequest, in flagArgs) (*mcp.CallToolResult, any, error) {
	msg, err := a.store.MessageByID(in.ID)
	if err != nil || msg == nil {
		return nil, nil, fmt.Errorf("no message with id %d", in.ID)
	}
	starred := in.Starred == nil || *in.Starred
	a.applyStarred(msg, starred)
	return structured(map[string]any{"id": msg.ID, "is_starred": starred})
}

type moveArgs struct {
	ID       int64  `json:"id" jsonschema:"the message id"`
	FolderID int64  `json:"folder_id,omitempty" jsonschema:"destination folder id (same account)"`
	Target   string `json:"target,omitempty" jsonschema:"or a special destination: archive, spam or trash"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"required true for target=trash — ask the human first"`
}

func (a *api) mcpMove(_ context.Context, _ *mcp.CallToolRequest, in moveArgs) (*mcp.CallToolResult, any, error) {
	msg, err := a.store.MessageByID(in.ID)
	if err != nil || msg == nil {
		return nil, nil, fmt.Errorf("no message with id %d", in.ID)
	}
	switch {
	case in.Target != "":
		if in.Target != "archive" && in.Target != "spam" && in.Target != "trash" {
			return nil, nil, fmt.Errorf("target must be archive, spam or trash")
		}
		if in.Target == "trash" && !in.Confirm {
			return nil, nil, fmt.Errorf("moving to trash needs confirm=true — ask the human before deleting mail")
		}
		fid, err := a.moveSpecial(msg, in.Target)
		if err != nil {
			return nil, nil, err
		}
		return structured(map[string]any{"id": msg.ID, "folder_id": fid, "target": in.Target})
	case in.FolderID != 0:
		tf, err := a.store.FolderByID(in.FolderID)
		if err != nil || tf == nil {
			return nil, nil, fmt.Errorf("no folder with id %d", in.FolderID)
		}
		if tf.Account != msg.Account {
			return nil, nil, fmt.Errorf("folder %d belongs to a different account", in.FolderID)
		}
		a.moveFolder(msg, tf)
		return structured(map[string]any{"id": msg.ID, "folder_id": tf.ID})
	}
	return nil, nil, fmt.Errorf("set folder_id or target")
}

// --- send tools (two-step by design: draft_reply previews, send_draft sends) ---

type draftReplyArgs struct {
	InReplyTo int64    `json:"in_reply_to,omitempty" jsonschema:"message id to reply to; omit for a fresh mail"`
	ReplyAll  bool     `json:"reply_all,omitempty" jsonschema:"include all original recipients"`
	To        []string `json:"to,omitempty" jsonschema:"recipients; derived from the original for replies"`
	Cc        []string `json:"cc,omitempty"`
	Bcc       []string `json:"bcc,omitempty"`
	Subject   string   `json:"subject,omitempty" jsonschema:"derived (Re: …) for replies when omitted"`
	Body      string   `json:"body" jsonschema:"the message text, plain text"`
	Account   string   `json:"account,omitempty" jsonschema:"sending account; derived from the original or the only account"`
}

type draftPreview struct {
	DraftID   int64    `json:"draft_id"`
	Account   string   `json:"account"`
	To        []string `json:"to"`
	Cc        []string `json:"cc,omitempty"`
	Bcc       []string `json:"bcc,omitempty"`
	Subject   string   `json:"subject"`
	Body      string   `json:"body"`
	InReplyTo string   `json:"in_reply_to,omitempty"`
	Note      string   `json:"note"`
}

func (a *api) mcpDraftReply(_ context.Context, _ *mcp.CallToolRequest, in draftReplyArgs) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Body) == "" {
		return nil, nil, fmt.Errorf("body is required")
	}
	d := &store.Draft{
		Account: in.Account, Body: in.Body, Mode: "plain", Kind: "new",
		To: strings.Join(in.To, ", "), Cc: strings.Join(in.Cc, ", "), Bcc: strings.Join(in.Bcc, ", "),
		Subject: in.Subject,
	}
	if in.InReplyTo != 0 {
		orig, err := a.store.MessageByID(in.InReplyTo)
		if err != nil || orig == nil {
			return nil, nil, fmt.Errorf("in_reply_to: no message with id %d", in.InReplyTo)
		}
		if d.Account == "" {
			d.Account = orig.Account
		}
		self := a.selfAddress(d.Account)
		if len(in.To) == 0 {
			if in.ReplyAll {
				to, cc := mail.ReplyAllRecipients(self, []string{orig.FromAddress},
					mail.SplitAddrList(orig.ToAddresses), mail.SplitAddrList(orig.CcAddresses))
				d.To, d.Cc = strings.Join(to, ", "), strings.Join(cc, ", ")
			} else {
				d.To = strings.Join(mail.ReplyRecipients(self, orig.FromAddress), ", ")
			}
		}
		if d.Subject == "" {
			d.Subject = mail.PrefixSubject("reply", orig.Subject)
		}
		d.InReplyTo = orig.MessageID
		d.Kind = "reply"
		if in.ReplyAll {
			d.Kind = "reply_all"
		}
	} else if len(in.To) == 0 {
		return nil, nil, fmt.Errorf("to is required for a fresh mail")
	}
	if d.Account == "" {
		if len(a.deps.Cfg.Accounts) == 1 {
			d.Account = a.deps.Cfg.Accounts[0].Name
		} else {
			return nil, nil, fmt.Errorf("set account — more than one is configured")
		}
	}
	if err := a.store.UpsertDraft(d); err != nil {
		return nil, nil, fmt.Errorf("couldn't save the draft: %w", err)
	}
	a.publishDraft(d)
	return structured(draftPreview{
		DraftID: d.ID, Account: d.Account,
		To: mail.SplitAddrList(d.To), Cc: mail.SplitAddrList(d.Cc), Bcc: mail.SplitAddrList(d.Bcc),
		Subject: d.Subject, Body: d.Body, InReplyTo: d.InReplyTo,
		Note: "Draft saved, NOT sent. Show this preview to the human; send with send_draft only after approval.",
	})
}

type sendDraftArgs struct {
	DraftID int64 `json:"draft_id" jsonschema:"the draft id from draft_reply"`
}

func (a *api) mcpSendDraft(ctx context.Context, _ *mcp.CallToolRequest, in sendDraftArgs) (*mcp.CallToolResult, any, error) {
	d, err := a.store.DraftByID(in.DraftID)
	if err != nil || d == nil {
		return nil, nil, fmt.Errorf("no draft with id %d (already sent?)", in.DraftID)
	}
	account := d.Account
	if account == "" {
		if len(a.deps.Cfg.Accounts) != 1 {
			return nil, nil, fmt.Errorf("draft has no account and more than one is configured")
		}
		account = a.deps.Cfg.Accounts[0].Name
	}
	in2 := mail.ComposeInput{
		To: mail.SplitAddrList(d.To), Cc: mail.SplitAddrList(d.Cc), Bcc: mail.SplitAddrList(d.Bcc),
		Subject: d.Subject, Body: d.Body, Mode: d.Mode, From: a.selfAddress(account),
	}
	if len(in2.To) == 0 {
		return nil, nil, fmt.Errorf("draft has no recipients")
	}
	if d.InReplyTo != "" {
		// Same fallback the compose form uses when resuming a draft: thread on
		// the parent alone rather than the full References chain.
		in2.InReplyTo = d.InReplyTo
		in2.References = "<" + d.InReplyTo + ">"
	}
	msgID, err := a.mail.Send(ctx, account, in2)
	if err != nil {
		return nil, nil, fmt.Errorf("send failed: %w", err)
	}
	_ = a.store.DeleteDraft(d.ID)
	// The Drafts folder copy has to go too, or the human's other clients keep
	// offering to resume a message that is already out.
	a.background("drop draft", func(ctx context.Context) error { return a.mail.DropDraft(ctx, d) })
	return structured(map[string]any{"status": "sent", "message_id": msgID, "draft_id": d.ID})
}

// --- resource ---

func (a *api) mcpInboxSummary(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	accounts, err := a.accountsView()
	if err != nil {
		return nil, err
	}
	msgs, _, err := a.store.ListUnifiedInboxPage("", 100)
	if err != nil {
		return nil, err
	}
	type unreadJSON struct {
		ID      int64     `json:"id"`
		Account string    `json:"account"`
		From    string    `json:"from"`
		Subject string    `json:"subject"`
		Date    time.Time `json:"date"`
	}
	unread := []unreadJSON{}
	var total int64
	for _, ac := range accounts {
		total += ac.Unread
	}
	for _, m := range msgs {
		if m.IsRead || len(unread) >= 20 {
			continue
		}
		from := m.FromName
		if from == "" {
			from = m.FromAddress
		}
		unread = append(unread, unreadJSON{ID: m.ID, Account: m.Account, From: from, Subject: m.Subject, Date: m.Date.UTC()})
	}
	b, err := json.Marshal(map[string]any{
		"accounts": accounts, "unread_total": total, "newest_unread": unread,
	})
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
		URI: req.Params.URI, MIMEType: "application/json", Text: string(b),
	}}}, nil
}

// --- small shared bits ---

// accountNames resolves the account filter: one name if it exists, or all.
func (a *api) accountNames(only string) []string {
	var names []string
	for _, ac := range a.deps.Cfg.Accounts {
		if only == "" || ac.Name == only {
			names = append(names, ac.Name)
		}
	}
	return names
}

// selfAddress is the account's own address, for reply-recipient computation.
func (a *api) selfAddress(account string) string {
	for _, ac := range a.deps.Cfg.Accounts {
		if ac.Name == account {
			return ac.Email
		}
	}
	return ""
}

func searchScopeFor(account string, folderID int64) search.Scope {
	switch {
	case folderID != 0:
		return search.ScopeFolder
	case account != "":
		return search.ScopeAccount
	}
	return search.ScopeAll
}

func clampLimit(n int) int {
	if n <= 0 {
		return searchLimit
	}
	if n > searchLimitMax {
		return searchLimitMax
	}
	return n
}
