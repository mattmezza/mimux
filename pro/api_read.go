//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// handleAccounts lists configured accounts with live sync state and stored-mail
// counters. Credentials never appear here — fields are picked, not marshalled
// off config.Account.
func (a *api) handleAccounts(w http.ResponseWriter, r *http.Request) {
	out, err := a.accountsView()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't read account stats.")
		return
	}
	writeList(w, out, "")
}

// accountJSON is the field-picked account view shared by the REST and MCP
// surfaces. config.Account itself carries credentials and is never marshalled.
type accountJSON struct {
	Name     string     `json:"name"`
	Email    string     `json:"email"`
	State    string     `json:"state,omitempty"`
	Message  string     `json:"message,omitempty"`
	LastSync *time.Time `json:"last_sync,omitempty"`
	Messages int64      `json:"messages"`
	Unread   int64      `json:"unread"`
	Folders  int64      `json:"folders"`
}

func (a *api) accountsView() ([]accountJSON, error) {
	stats, err := a.store.AccountStats()
	if err != nil {
		return nil, err
	}
	status := map[string]mail.AccountStatus{}
	for _, st := range a.mail.Status() {
		status[st.Account] = st
	}
	out := []accountJSON{}
	for _, ac := range a.deps.Cfg.Accounts {
		st := stats[ac.Name]
		j := accountJSON{
			Name: ac.Name, Email: ac.Email,
			Messages: st.Messages, Unread: st.Unread, Folders: st.Folders,
		}
		if s, ok := status[ac.Name]; ok {
			j.State, j.Message = s.State, s.Message
			if !s.LastSync.IsZero() {
				t := s.LastSync.UTC()
				j.LastSync = &t
			}
		}
		out = append(out, j)
	}
	return out, nil
}

// folderNodeJSON mirrors store.FolderNode with only the fields an API caller
// needs; intermediate path segments have no folder object.
type folderNodeJSON struct {
	Label      string           `json:"label"`
	ID         int64            `json:"id,omitempty"`
	Name       string           `json:"name,omitempty"`
	SpecialUse string           `json:"special_use,omitempty"`
	Unread     int              `json:"unread,omitempty"`
	Children   []folderNodeJSON `json:"children,omitempty"`
}

func toFolderNodes(nodes []*store.FolderNode) []folderNodeJSON {
	out := make([]folderNodeJSON, 0, len(nodes))
	for _, n := range nodes {
		j := folderNodeJSON{Label: n.Label, Children: toFolderNodes(n.Children)}
		if n.Folder != nil {
			j.ID, j.Name, j.SpecialUse, j.Unread = n.Folder.ID, n.Folder.Name, n.Folder.SpecialUse, n.Folder.UnreadCount
		}
		out = append(out, j)
	}
	return out
}

// handleFolders returns each account's folder tree (?account= narrows to one).
func (a *api) handleFolders(w http.ResponseWriter, r *http.Request) {
	names := []string{}
	if q := r.URL.Query().Get("account"); q != "" {
		names = append(names, q)
	} else {
		for _, ac := range a.deps.Cfg.Accounts {
			names = append(names, ac.Name)
		}
	}
	out, err := a.foldersView(names)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't list folders.")
		return
	}
	writeList(w, out, "")
}

// accountFolders is one account's folder tree, shared by REST and MCP.
type accountFolders struct {
	Account string           `json:"account"`
	Folders []folderNodeJSON `json:"folders"`
}

func (a *api) foldersView(names []string) ([]accountFolders, error) {
	out := []accountFolders{}
	for _, name := range names {
		folders, err := a.store.ListFolders(name)
		if err != nil {
			return nil, err
		}
		out = append(out, accountFolders{Account: name, Folders: toFolderNodes(store.FolderTree(folders))})
	}
	return out, nil
}

const defaultPageLimit = 100
const maxPageLimit = 500

// handleListMessages lists messages newest-first with cursor pagination.
// Scope: ?folder=<id>, else ?account=<name>'s inbox, else the unified inbox.
// It pages by MESSAGE (store.messagePage), not by conversation the way the web
// UI's list does: limit here is a promise about the size of the response, so a
// page holds at most limit rows. Filters (unread, starred, since, from, to,
// subject) are applied to each underlying page, so a filtered response may hold
// fewer than limit items — keep following next_cursor until it comes back
// empty. For arbitrary queries use POST /messages/search, which speaks the full
// query language.
func (a *api) handleListMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := defaultPageLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageLimit {
			apiError(w, http.StatusBadRequest, "invalid_request", "limit must be 1-"+strconv.Itoa(maxPageLimit)+".")
			return
		}
		limit = n
	}
	cursor := q.Get("cursor")

	var msgs []store.Message
	var next string
	var err error
	switch {
	case q.Get("folder") != "":
		fid, perr := strconv.ParseInt(q.Get("folder"), 10, 64)
		if perr != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "folder must be a folder id.")
			return
		}
		if f, ferr := a.store.FolderByID(fid); ferr != nil || f == nil {
			apiError(w, http.StatusNotFound, "not_found", "No folder with id "+q.Get("folder")+".")
			return
		}
		msgs, next, err = a.store.ListMessagesPageByMessage(fid, cursor, limit)
	case q.Get("account") != "":
		f, ferr := a.store.FolderBySpecial(q.Get("account"), "inbox")
		if ferr != nil || f == nil {
			apiError(w, http.StatusNotFound, "not_found", "No inbox folder for account "+q.Get("account")+".")
			return
		}
		msgs, next, err = a.store.ListMessagesPageByMessage(f.ID, cursor, limit)
	default:
		msgs, next, err = a.store.ListUnifiedInboxPageByMessage(cursor, limit)
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't list messages.")
		return
	}

	// Thread annotation, the same cheap way the HTML list does it: thread the
	// page, stamp each member with its thread's key and whole-conversation size.
	// thread_size stays whole-conversation because ConversationSizes is scored
	// over the whole store, not over the page. thread_id is only page-local: a
	// conversation straddling a page boundary can be rooted differently on each
	// side, so group by it within a page, not across pages. Message paging makes
	// that more likely than conversation paging did, not new — a conversation
	// with an unread member already spanned pages before.
	threadOf := map[int64]int64{}
	threadSize := map[int64]int{}
	sizes, _ := a.store.ConversationSizes()
	for _, t := range mail.BuildThreads(msgs) {
		size := t.Count
		if s := sizes[t.RootID()]; s > size {
			size = s
		}
		for _, m := range t.Messages {
			threadOf[m.ID] = t.RootID()
			threadSize[m.ID] = size
		}
	}

	filter := newListFilter(q)
	out := []messageJSON{}
	for _, m := range msgs {
		if !filter.match(m) {
			continue
		}
		j := toMessageJSON(m)
		j.ThreadID = threadOf[m.ID]
		j.ThreadSize = threadSize[m.ID]
		out = append(out, j)
	}
	writeList(w, out, next)
}

// listFilter is the simple per-page predicate behind GET /messages' filter
// params.
type listFilter struct {
	unread, starred   *bool
	since             time.Time
	from, to, subject string
}

func newListFilter(q map[string][]string) listFilter {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	f := listFilter{
		from:    strings.ToLower(get("from")),
		to:      strings.ToLower(get("to")),
		subject: strings.ToLower(get("subject")),
	}
	parseBool := func(k string) *bool {
		switch get(k) {
		case "true", "1":
			b := true
			return &b
		case "false", "0":
			b := false
			return &b
		}
		return nil
	}
	f.unread = parseBool("unread")
	f.starred = parseBool("starred")
	if v := get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.since = t
		}
	}
	return f
}

func (f listFilter) match(m store.Message) bool {
	if f.unread != nil && m.IsRead == *f.unread {
		return false
	}
	if f.starred != nil && m.IsStarred != *f.starred {
		return false
	}
	if !f.since.IsZero() && m.Date.Before(f.since) {
		return false
	}
	if f.from != "" && !strings.Contains(strings.ToLower(m.FromName+" "+m.FromAddress), f.from) {
		return false
	}
	if f.to != "" && !strings.Contains(strings.ToLower(m.ToAddresses+" "+m.CcAddresses), f.to) {
		return false
	}
	if f.subject != "" && !strings.Contains(strings.ToLower(m.Subject), f.subject) {
		return false
	}
	return true
}

// handleGetMessage returns one full message. ?body=text|html|both picks the
// body forms (default text); the HTML form is the same sanitized document the
// reading pane renders. ?headers=raw|parsed|both adds the message's own header
// block, opt-in because a full Received: chain is bulk nobody asked for. Neither
// failing to fetch (account offline) fails the request — metadata still
// returns, with body_error / headers_error set.
func (a *api) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	msg := a.messageOr404(w, r)
	if msg == nil {
		return
	}
	want := r.URL.Query().Get("body")
	if want == "" {
		want = "text"
	}
	if want != "text" && want != "html" && want != "both" {
		apiError(w, http.StatusBadRequest, "invalid_request", "body must be text, html or both.")
		return
	}
	wantHeaders := r.URL.Query().Get("headers")
	if wantHeaders != "" && wantHeaders != "raw" && wantHeaders != "parsed" && wantHeaders != "both" {
		apiError(w, http.StatusBadRequest, "invalid_request", "headers must be raw, parsed or both.")
		return
	}

	type bodyJSON struct {
		Text string `json:"text,omitempty"`
		HTML string `json:"html,omitempty"`
	}
	type headersJSON struct {
		Raw    string              `json:"raw,omitempty"`
		Parsed map[string][]string `json:"parsed,omitempty"`
	}
	type attachmentJSON struct {
		Index     int    `json:"index"`
		Filename  string `json:"filename"`
		MediaType string `json:"media_type"`
		Size      uint32 `json:"size"`
	}
	out := struct {
		messageJSON
		Body         *bodyJSON        `json:"body,omitempty"`
		BodyError    string           `json:"body_error,omitempty"`
		Headers      *headersJSON     `json:"headers,omitempty"`
		HeadersError string           `json:"headers_error,omitempty"`
		Attachments  []attachmentJSON `json:"attachments"`
	}{messageJSON: toMessageJSON(*msg), Attachments: []attachmentJSON{}}

	var body bodyJSON
	var bodyErr error
	if want == "text" || want == "both" {
		body.Text, bodyErr = a.mail.PlainText(r.Context(), msg)
	}
	if want == "html" || want == "both" {
		var err error
		// External images allowed: the caller is a machine reading JSON, not a
		// tracked browser render; the sanitizer still strips active content.
		body.HTML, _, err = a.mail.Body(r.Context(), msg, true, false)
		if bodyErr == nil {
			bodyErr = err
		}
	}
	if bodyErr != nil {
		out.BodyError = "Couldn't fetch the body — the account may be offline."
	} else {
		out.Body = &body
	}

	if wantHeaders != "" {
		raw, parsed, err := a.mail.Headers(r.Context(), msg)
		if err != nil {
			out.HeadersError = "Couldn't fetch the headers — the account may be offline."
		} else {
			var headers headersJSON
			if wantHeaders == "raw" || wantHeaders == "both" {
				headers.Raw = raw
			}
			if wantHeaders == "parsed" || wantHeaders == "both" {
				headers.Parsed = parsed
			}
			out.Headers = &headers
		}
	}

	if msg.HasAttachment {
		atts, err := a.mail.Attachments(r.Context(), msg)
		if err == nil {
			for i, at := range atts {
				out.Attachments = append(out.Attachments, attachmentJSON{
					Index: i, Filename: at.Filename, MediaType: at.MediaType, Size: at.Size,
				})
			}
		}
	}
	writeJSON(w, out)
}

func (a *api) handleMessageHeaders(w http.ResponseWriter, r *http.Request) {
	msg := a.messageOr404(w, r)
	if msg == nil {
		return
	}
	raw, parsed, err := a.mail.Headers(r.Context(), msg)
	if err != nil {
		apiError(w, http.StatusBadGateway, "upstream", "Couldn't fetch the headers — the account may be offline.")
		return
	}
	writeJSON(w, map[string]any{"raw": raw, "parsed": parsed})
}

func (a *api) handleRawMessage(w http.ResponseWriter, r *http.Request) {
	msg := a.messageOr404(w, r)
	if msg == nil {
		return
	}
	raw, err := a.mail.Raw(r.Context(), msg)
	if err != nil {
		apiError(w, http.StatusBadGateway, "upstream", "Couldn't fetch the raw message — the account may be offline.")
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(mail.MessageFilename(msg.Subject, msg.ID), `"`, "")+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	// #nosec G705 -- exact message bytes are deliberately downloaded with an
	// attachment disposition and nosniff; they are never interpreted as HTML.
	_, _ = w.Write(raw)
}

// handleAttachment streams attachment #n (the index from GET /messages/{id})
// with its real content type.
func (a *api) handleAttachment(w http.ResponseWriter, r *http.Request) {
	msg := a.messageOr404(w, r)
	if msg == nil {
		return
	}
	n, err := strconv.Atoi(chi.URLParam(r, "n"))
	if err != nil || n < 0 {
		apiError(w, http.StatusNotFound, "not_found", "No such attachment.")
		return
	}
	atts, err := a.mail.Attachments(r.Context(), msg)
	if err != nil {
		apiError(w, http.StatusBadGateway, "upstream", "Couldn't list attachments — the account may be offline.")
		return
	}
	if n >= len(atts) {
		apiError(w, http.StatusNotFound, "not_found", "Message has "+strconv.Itoa(len(atts))+" attachments.")
		return
	}
	data, mediaType, filename, err := a.mail.Attachment(r.Context(), msg, atts[n].Part)
	if err != nil {
		apiError(w, http.StatusBadGateway, "upstream", "Couldn't fetch the attachment.")
		return
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(filename, `"`, "")+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}
