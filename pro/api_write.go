//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/filter"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// maxAttachTotal caps the combined attachment size on one message — the same
// 25MB the compose form enforces.
const maxAttachTotal = 25 << 20

func validAPIMode(v string) (string, bool) {
	switch v {
	case "":
		return "plain", true
	case "plain", "html", "markdown":
		return v, true
	}
	return "", false
}

type sendAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Data        string `json:"data"` // base64
}

type sendRequest struct {
	Account     string           `json:"account"`
	From        string           `json:"from"`
	To          []string         `json:"to"`
	Cc          []string         `json:"cc"`
	Bcc         []string         `json:"bcc"`
	Subject     string           `json:"subject"`
	Body        string           `json:"body"`
	Mode        string           `json:"mode"`
	InReplyTo   int64            `json:"in_reply_to"` // message id, not a header value
	Attachments []sendAttachment `json:"attachments"`
	ScheduleAt  *time.Time       `json:"schedule_at"`
}

// resolveAccount picks the sending account: explicit name, then the owner of
// the from address, then the replied-to message's account, then the only
// configured account. Ambiguity is an error, not a guess.
func (a *api) resolveAccount(req *sendRequest, orig *store.Message) (config.Account, bool) {
	accounts := a.deps.Cfg.Accounts
	if req.Account != "" {
		for _, ac := range accounts {
			if ac.Name == req.Account {
				return ac, true
			}
		}
		return config.Account{}, false
	}
	if req.From != "" {
		if ac, ok := config.AccountForAddress(accounts, req.From); ok {
			return ac, true
		}
		return config.Account{}, false
	}
	if orig != nil {
		for _, ac := range accounts {
			if ac.Name == orig.Account {
				return ac, true
			}
		}
	}
	if len(accounts) == 1 {
		return accounts[0], true
	}
	return config.Account{}, false
}

// handleSend sends a message now, or enqueues it when schedule_at is set.
// in_reply_to takes a mimux message id and derives the threading headers the
// way the compose form does.
func (a *api) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.To) == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "to must have at least one recipient.")
		return
	}
	mode, ok := validAPIMode(req.Mode)
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid_request", "mode must be plain, html or markdown.")
		return
	}

	var orig *store.Message
	if req.InReplyTo != 0 {
		var err error
		orig, err = a.store.MessageByID(req.InReplyTo)
		if err != nil || orig == nil {
			apiError(w, http.StatusNotFound, "not_found", "in_reply_to: no message with that id.")
			return
		}
	}
	ac, ok := a.resolveAccount(&req, orig)
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid_request", "Couldn't resolve the sending account — set account or from.")
		return
	}
	from := req.From
	if from == "" {
		from = ac.Email
	}

	atts, attErr := decodeAttachments(req.Attachments)
	if attErr != "" {
		apiError(w, http.StatusBadRequest, "invalid_request", attErr)
		return
	}

	in := mail.ComposeInput{
		To: req.To, Cc: req.Cc, Bcc: req.Bcc,
		Subject: req.Subject, Body: req.Body, Mode: mode, From: from,
		Attachments: atts,
	}
	if orig != nil {
		in.InReplyTo = orig.MessageID
		in.References = mail.ComputeReferences(orig.Refs, orig.MessageID)
	}

	if req.ScheduleAt != nil {
		o := &store.Outbox{
			Account: ac.Name, From: from,
			To: strings.Join(req.To, ", "), Cc: strings.Join(req.Cc, ", "), Bcc: strings.Join(req.Bcc, ", "),
			Subject: req.Subject, Body: req.Body, Mode: mode,
			InReplyTo: in.InReplyTo, References: in.References,
			Attachments: outAttachments(atts), SendAt: req.ScheduleAt.UTC(),
		}
		if err := a.store.EnqueueOutbox(o); err != nil {
			apiError(w, http.StatusInternalServerError, "internal", "Couldn't queue the message.")
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]any{"status": "scheduled", "outbox_id": o.ID, "send_at": o.SendAt.Format(time.RFC3339)})
		return
	}

	msgID, err := a.mail.Send(r.Context(), ac.Name, in)
	if err != nil {
		// The error text is safe to relay: it's an SMTP/connection failure, and
		// the single user owns both sides of this conversation.
		apiError(w, http.StatusBadGateway, "send_failed", "Send failed: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"status": "sent", "message_id": msgID})
}

func decodeAttachments(in []sendAttachment) ([]mail.OutAttachment, string) {
	var out []mail.OutAttachment
	var total int
	for _, at := range in {
		if at.Filename == "" {
			return nil, "Every attachment needs a filename."
		}
		data, err := base64.StdEncoding.DecodeString(at.Data)
		if err != nil {
			return nil, "Attachment " + at.Filename + ": data is not valid base64."
		}
		total += len(data)
		if total > maxAttachTotal {
			return nil, "Attachments exceed the " + strconv.Itoa(maxAttachTotal>>20) + "MB limit."
		}
		out = append(out, mail.OutAttachment{Filename: at.Filename, ContentType: at.ContentType, Data: data})
	}
	return out, ""
}

func outAttachments(atts []mail.OutAttachment) []store.OutAttachment {
	out := make([]store.OutAttachment, len(atts))
	for i, at := range atts {
		out[i] = store.OutAttachment{Filename: at.Filename, ContentType: at.ContentType, Data: at.Data}
	}
	return out
}

// --- drafts ---

type draftRequest struct {
	Account   string `json:"account"`
	To        string `json:"to"`
	Cc        string `json:"cc"`
	Bcc       string `json:"bcc"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	Mode      string `json:"mode"`
	Kind      string `json:"kind"`
	InReplyTo int64  `json:"in_reply_to"` // message id, resolved to its Message-ID header
}

type draftJSON struct {
	ID        int64     `json:"id"`
	Account   string    `json:"account"`
	To        string    `json:"to"`
	Cc        string    `json:"cc,omitempty"`
	Bcc       string    `json:"bcc,omitempty"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	Mode      string    `json:"mode,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toDraftJSON(d store.Draft) draftJSON {
	return draftJSON{
		ID: d.ID, Account: d.Account, To: d.To, Cc: d.Cc, Bcc: d.Bcc,
		Subject: d.Subject, Body: d.Body, Mode: d.Mode, Kind: d.Kind,
		UpdatedAt: d.UpdatedAt.UTC(),
	}
}

func (a *api) handleCreateDraft(w http.ResponseWriter, r *http.Request) {
	var req draftRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	mode, ok := validAPIMode(req.Mode)
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid_request", "mode must be plain, html or markdown.")
		return
	}
	d := &store.Draft{
		Account: req.Account, To: req.To, Cc: req.Cc, Bcc: req.Bcc,
		Subject: req.Subject, Body: req.Body, Mode: mode, Kind: req.Kind,
	}
	if d.Kind == "" {
		d.Kind = "new"
	}
	if req.InReplyTo != 0 {
		orig, err := a.store.MessageByID(req.InReplyTo)
		if err != nil || orig == nil {
			apiError(w, http.StatusNotFound, "not_found", "in_reply_to: no message with that id.")
			return
		}
		d.InReplyTo = orig.MessageID
		if d.Account == "" {
			d.Account = orig.Account
		}
	}
	if err := a.store.UpsertDraft(d); err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't save the draft.")
		return
	}
	a.publishDraft(d)
	writeJSONStatus(w, http.StatusCreated, toDraftJSON(*d))
}

// handleUpdateDraft merges the provided fields into an existing draft; absent
// fields keep their stored value.
func (a *api) handleUpdateDraft(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	d, err := a.store.DraftByID(id)
	if err != nil || d == nil {
		apiError(w, http.StatusNotFound, "not_found", "No draft with id "+strconv.FormatInt(id, 10)+".")
		return
	}
	var req struct {
		Account *string `json:"account"`
		To      *string `json:"to"`
		Cc      *string `json:"cc"`
		Bcc     *string `json:"bcc"`
		Subject *string `json:"subject"`
		Body    *string `json:"body"`
		Mode    *string `json:"mode"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&d.Account, req.Account)
	set(&d.To, req.To)
	set(&d.Cc, req.Cc)
	set(&d.Bcc, req.Bcc)
	set(&d.Subject, req.Subject)
	set(&d.Body, req.Body)
	if req.Mode != nil {
		mode, ok := validAPIMode(*req.Mode)
		if !ok {
			apiError(w, http.StatusBadRequest, "invalid_request", "mode must be plain, html or markdown.")
			return
		}
		d.Mode = mode
	}
	if err := a.store.UpsertDraft(d); err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't save the draft.")
		return
	}
	a.publishDraft(d)
	writeJSON(w, toDraftJSON(*d))
}

// publishDraft appends a draft saved through the API to the account's IMAP
// Drafts folder, so an agent's draft shows up in the human's mail client. Store
// first, IMAP in the background — the same shape as handlePatchMessage, and the
// row already carries the "push still owed" marker the sync worker retries.
func (a *api) publishDraft(d *store.Draft) {
	if d.ID == 0 || d.Account == "" {
		return
	}
	dc := *d
	a.background("push draft", func(ctx context.Context) error { return a.mail.PushDraft(ctx, &dc) })
}

// --- mutations ---

// handlePatchMessage applies read/star/label/move changes. Store first, IMAP
// write in the background — the same optimistic pattern as the HTML client,
// minus its 11-second undo timer: an API caller that asks for a move means it,
// so the server-side move fires immediately.
func (a *api) handlePatchMessage(w http.ResponseWriter, r *http.Request) {
	msg := a.messageOr404(w, r)
	if msg == nil {
		return
	}
	var req struct {
		Read         *bool    `json:"read"`
		Starred      *bool    `json:"starred"`
		LabelsAdd    []string `json:"labels_add"`
		LabelsRemove []string `json:"labels_remove"`
		FolderID     *int64   `json:"folder_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Read != nil {
		a.applyRead(msg, *req.Read)
	}
	if req.Starred != nil {
		a.applyStarred(msg, *req.Starred)
	}
	for _, l := range req.LabelsAdd {
		if err := a.mail.SetLabel(msg, l, true); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "Couldn't add label "+l+".")
			return
		}
	}
	for _, l := range req.LabelsRemove {
		if err := a.mail.SetLabel(msg, l, false); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "Couldn't remove label "+l+".")
			return
		}
	}
	if req.FolderID != nil {
		tf, err := a.store.FolderByID(*req.FolderID)
		if err != nil || tf == nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "folder_id: no such folder.")
			return
		}
		if tf.Account != msg.Account {
			apiError(w, http.StatusBadRequest, "invalid_request", "folder_id belongs to a different account.")
			return
		}
		if tf.ID != msg.FolderID {
			a.moveFolder(msg, tf)
		}
	}

	fresh, err := a.store.MessageByID(msg.ID)
	if err != nil || fresh == nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't re-read the message.")
		return
	}
	writeJSON(w, toMessageJSON(*fresh))
}

// handleMove is archive/spam/delete: a move to the account's special-use
// folder, immediate (no undo window — see handlePatchMessage).
func (a *api) handleMove(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		msg := a.messageOr404(w, r)
		if msg == nil {
			return
		}
		fid, err := a.moveSpecial(msg, target)
		if err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		writeJSON(w, map[string]any{"status": "ok", "folder_id": fid})
	}
}

// --- optimistic mutation helpers, shared by the REST handlers and the MCP
// tools: store first, IMAP write in the background (see handlePatchMessage). ---

func (a *api) applyRead(msg *store.Message, read bool) {
	_ = a.store.SetRead(msg.ID, read)
	_ = a.store.RecountUnread(msg.FolderID)
	msg.IsRead = read
	msgCopy := *msg
	a.background("set read", func(ctx context.Context) error { return a.mail.SetRead(ctx, &msgCopy, read) })
}

func (a *api) applyStarred(msg *store.Message, starred bool) {
	_, _ = a.store.SetStarred(msg.ID, starred)
	msg.IsStarred = starred
	msgCopy := *msg
	a.background("set starred", func(ctx context.Context) error { return a.mail.SetStarred(ctx, &msgCopy, starred) })
}

// moveFolder moves msg to tf, which the caller has already validated to exist
// in the same account and differ from the current folder.
func (a *api) moveFolder(msg *store.Message, tf *store.Folder) {
	src := msg.FolderID
	_ = a.store.SetMessageFolderPending(msg.ID, tf.ID)
	_ = a.store.RecountUnread(src)
	_ = a.store.RecountUnread(tf.ID)
	msgCopy := *msg
	target := tf.Name
	a.background("move", func(ctx context.Context) error { return a.mail.MoveToFolder(ctx, &msgCopy, target) })
	msg.FolderID = tf.ID
}

// moveSpecial moves msg to its account's special-use folder (archive, spam,
// trash), returning the destination folder id.
func (a *api) moveSpecial(msg *store.Message, target string) (int64, error) {
	tf, err := a.store.FolderBySpecial(msg.Account, target)
	if err != nil || tf == nil {
		return 0, fmt.Errorf("this account has no %s folder", target)
	}
	src := msg.FolderID
	_ = a.store.SetMessageFolderPending(msg.ID, tf.ID)
	_ = a.store.RecountUnread(src)
	_ = a.store.RecountUnread(tf.ID)
	msgCopy := *msg
	a.background(target, func(ctx context.Context) error { return a.mail.Move(ctx, &msgCopy, target) })
	return tf.ID, nil
}

// --- filters ---

func (a *api) handleListFilters(w http.ResponseWriter, r *http.Request) {
	rules, err := a.store.ListRules()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't list filters.")
		return
	}
	if rules == nil {
		rules = []filter.Rule{}
	}
	writeList(w, rules, "")
}

func (a *api) handleCreateFilter(w http.ResponseWriter, r *http.Request) {
	var rule filter.Rule
	if !decodeJSON(w, r, &rule) {
		return
	}
	rule.ID = 0
	if err := rule.Validate(); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := a.store.CreateRule(&rule); err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't create the filter.")
		return
	}
	writeJSONStatus(w, http.StatusCreated, rule)
}
