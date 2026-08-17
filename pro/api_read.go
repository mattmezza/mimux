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
	stats, err := a.store.AccountStats()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't read account stats.")
		return
	}
	status := map[string]mail.AccountStatus{}
	for _, st := range a.mail.Status() {
		status[st.Account] = st
	}
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
	writeList(w, out, "")
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
	type accountFolders struct {
		Account string           `json:"account"`
		Folders []folderNodeJSON `json:"folders"`
	}
	out := []accountFolders{}
	for _, name := range names {
		folders, err := a.store.ListFolders(name)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "internal", "Couldn't list folders.")
			return
		}
		out = append(out, accountFolders{Account: name, Folders: toFolderNodes(store.FolderTree(folders))})
	}
	writeList(w, out, "")
}

const defaultPageLimit = 100
const maxPageLimit = 500

// handleListMessages lists messages newest-first with cursor pagination.
// Scope: ?folder=<id>, else ?account=<name>'s inbox, else the unified inbox.
// Filters (unread, starred, since, from, to, subject) are applied to each
// underlying page, so a filtered response may hold fewer than limit items —
// keep following next_cursor until it comes back empty. For arbitrary queries
// use POST /messages/search, which speaks the full query language.
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
		msgs, next, err = a.store.ListMessagesPage(fid, cursor, limit)
	case q.Get("account") != "":
		f, ferr := a.store.FolderBySpecial(q.Get("account"), "inbox")
		if ferr != nil || f == nil {
			apiError(w, http.StatusNotFound, "not_found", "No inbox folder for account "+q.Get("account")+".")
			return
		}
		msgs, next, err = a.store.ListMessagesPage(f.ID, cursor, limit)
	default:
		msgs, next, err = a.store.ListUnifiedInboxPage(cursor, limit)
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal", "Couldn't list messages.")
		return
	}

	// Thread annotation, the same cheap way the HTML list does it: thread the
	// page, stamp each member with its thread's key and whole-conversation size.
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
// reading pane renders. A body that can't be fetched (account offline) doesn't
// fail the request — metadata still returns, with body_error set.
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

	type bodyJSON struct {
		Text string `json:"text,omitempty"`
		HTML string `json:"html,omitempty"`
	}
	type attachmentJSON struct {
		Index     int    `json:"index"`
		Filename  string `json:"filename"`
		MediaType string `json:"media_type"`
		Size      uint32 `json:"size"`
	}
	out := struct {
		messageJSON
		Body        *bodyJSON        `json:"body,omitempty"`
		BodyError   string           `json:"body_error,omitempty"`
		Attachments []attachmentJSON `json:"attachments"`
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
