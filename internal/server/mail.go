package server

import (
	"context"
	"encoding/json"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/sm/internal/auth"
	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/mail"
	"github.com/mattmezza/sm/internal/store"
)

const listLimit = 200

// templateFuncs are helpers available to every template.
var templateFuncs = template.FuncMap{
	"avatarColor":    mail.AvatarColor,
	"avatarInitials": mail.AvatarInitials,
	"faviconURL":     mail.FaviconURL,
	"relTime":        relTime,
	"untilTime":      untilTime,
	"outSnippet":     outSnippet,
	"folderLabel":    folderLabel,
	"folderTree":     folderTree,
	"messageLabels":  mail.MessageLabels,
	"dict":           dict,
	"highlight":      highlight,
	"identities":     identities,
	"receivedAlias":  receivedAlias,
	"shortURL":       shortURL,
	"composeHTML":    composeHTML,
	"toJSON":         toJSON,
}

// toJSON marshals a value to a compact JSON string for embedding in a data-*
// attribute (html/template escapes the result for the attribute context).
func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// composeHTML sanitizes stored/prefilled WYSIWYG body HTML and marks it safe to
// drop into the contenteditable editor unescaped. Sanitizing on the way out
// keeps a hand-edited draft row from injecting script into the compose page.
func composeHTML(s string) template.HTML {
	return template.HTML(mail.SanitizeComposeHTML(s)) //nolint:gosec // sanitized above
}

// shortURL renders scheme+host + a truncated tail of the path/query for
// display, so long tokenized unsubscribe links don't blow up a modal's width.
func shortURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	prefix := u.Scheme + "://" + u.Host
	rest := strings.TrimPrefix(raw, prefix)
	const tailLen = 8
	if len(rest) > tailLen+2 {
		rest = "/…" + rest[len(rest)-tailLen:]
	}
	return prefix + rest
}

// quickActionLists resolves the QuickActions preference into the ordered
// bar/menu id lists for the templates, dropping ids not in `supported` (nil =
// all) and the translate action when no translate API key is configured.
func (s *Server) quickActionLists(pref string, supported map[string]bool) (bar, menu []string) {
	keep := func(ids []string) []string {
		out := ids[:0]
		for _, id := range ids {
			if id == "translate" && s.store.GetAppConfig().TranslateAPIKey == "" {
				continue
			}
			if supported != nil && !supported[id] {
				continue
			}
			out = append(out, id)
		}
		return out
	}
	bar, menu = store.SplitQuickActions(pref)
	return keep(bar), keep(menu)
}

// threadMsgActions builds the per-message action row shown in each expanded
// thread message: the configured-visible actions (bar then menu order) except
// archive, with dark, refetch and translate(when enabled) always included —
// they replace the old per-message text-link footer, so they show even if the
// user placed them Hidden. NOTE: bar∪menu order, not a separate pref.
func threadMsgActions(bar, menu []string, translate bool) []string {
	seen := map[string]bool{"archive": true}
	var out []string
	add := func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range bar {
		add(id)
	}
	for _, id := range menu {
		add(id)
	}
	add("dark")
	add("refetch")
	if translate {
		add("translate")
	}
	return out
}

// accountAliases maps account name -> its configured alias addresses, set once
// at server construction (setAccountAliases). Lets the pure receivedAlias
// template func flag "received via <alias>" without per-request plumbing.
// NOTE: process-global because config is static and the app is single-user;
// rebuild it if config ever reloads at runtime.
var accountAliases = map[string][]string{}

func setAccountAliases(accts []config.Account) {
	m := map[string][]string{}
	for _, a := range accts {
		var emails []string
		for _, al := range a.Aliases {
			if al.Email != "" {
				emails = append(emails, al.Email)
			}
		}
		if len(emails) > 0 {
			m[a.Name] = emails
		}
	}
	accountAliases = m
}

// receivedAlias returns the account alias a message was delivered to (present
// in its To/Cc), or "" when it arrived at the account's primary address.
// NOTE: case-insensitive substring match of each bare alias against the
// joined To/Cc — enough to flag an alias in the UI; switch to per-address
// parsing if a substring false-positive ever surfaces.
func receivedAlias(account, to, cc string) string {
	aliases := accountAliases[account]
	if len(aliases) == 0 {
		return ""
	}
	hay := strings.ToLower(to + ", " + cc)
	for _, al := range aliases {
		if a := strings.ToLower(bareEmail(al)); a != "" && strings.Contains(hay, a) {
			return al
		}
	}
	return ""
}

// dict builds a map from alternating key/value args, for passing extra context
// into a partial inside a range (Go templates have no built-in for this).
func dict(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			m[k] = kv[i+1]
		}
	}
	return m
}

// sidebarAccount groups an account's folders for the sidebar tree.
type sidebarAccount struct {
	Name      string
	Folders   []store.Folder
	NeedsAuth bool // oauth2 account with no stored token yet → show "Connect"
}

func (s *Server) sidebarData() []sidebarAccount {
	out := make([]sidebarAccount, 0, len(s.cfg.Accounts))
	for _, ac := range s.cfg.Accounts {
		folders, _ := s.store.ListFolders(ac.Name)
		needsAuth := false
		if ac.Auth == "oauth2" {
			if tok, _ := s.store.GetToken(ac.Name); tok == nil || (tok.Access == "" && tok.Refresh == "") {
				needsAuth = true
			}
		}
		out = append(out, sidebarAccount{Name: ac.Name, Folders: folders, NeedsAuth: needsAuth})
	}
	return out
}

// fillList populates a template data map with a threaded message list. folder
// is nil for the unified view.
func (s *Server) fillList(data map[string]any, folder *store.Folder, msgs []store.Message, unified bool) {
	// The htmx-swapped list fragment (handleFolder/handleUnified) has no Prefs in
	// its data; supply them so rows can read badge/favicon prefs and colors. The
	// full-page path (handleInbox) already set these, so don't re-query there.
	if _, ok := data["Prefs"]; !ok {
		prefs := s.store.GetPrefs()
		data["Prefs"] = prefs
		data["AccountColors"] = prefs.AccountColors
	}
	data["Folder"] = folder
	data["Unified"] = unified
	data["Threads"] = mail.BuildThreads(msgs)
	data["HasMessages"] = len(msgs) > 0
	if unified {
		data["ListURL"] = "/u"
		data["Src"] = "u"
	} else if folder != nil {
		data["CurrentFolder"] = folder.ID
		data["ListURL"] = "/f/" + strconv.FormatInt(folder.ID, 10)
		data["Src"] = strconv.FormatInt(folder.ID, 10)
	}
}

// --- handlers ---

func (s *Server) handleStatusbar(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "statusbar", map[string]any{"Statuses": s.mail.Status()})
}

// handleHealth re-renders the sidebar accounts-health rows (live-updated over
// SSE when sync status changes).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "health_rows", s.mail.Status())
}

// handleRefresh triggers an immediate sync across every account.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.mail.RefreshAll()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleFolder(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	f, err := s.store.FolderByID(id)
	if err != nil || f == nil {
		http.NotFound(w, r)
		return
	}
	msgs, _ := s.store.ListMessages(f.ID, listLimit)
	data := map[string]any{"CSRF": auth.EnsureCSRF(w, r, s.secure)}
	s.fillList(data, f, msgs, false)
	s.renderPartial(w, "message_list", data)
}

// handleUnified renders the "All inboxes" threaded list partial.
func (s *Server) handleUnified(w http.ResponseWriter, r *http.Request) {
	msgs, _ := s.store.ListUnifiedInbox(listLimit)
	data := map[string]any{"CSRF": auth.EnsureCSRF(w, r, s.secure)}
	s.fillList(data, nil, msgs, true)
	s.renderPartial(w, "message_list", data)
}

// handleThread renders a whole conversation in the reading pane. The {id} is
// the thread's latest-message id; src selects the scope to re-thread ("u" for
// unified, else a folder id) so the same message groups the same way as the
// list it was opened from.
func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var msgs []store.Message
	src := r.URL.Query().Get("src")
	if src == "u" {
		msgs, _ = s.store.ListUnifiedInbox(listLimit)
	} else if fid, err := strconv.ParseInt(src, 10, 64); err == nil {
		msgs, _ = s.store.ListMessages(fid, listLimit)
	}
	var thread *mail.Thread
	for _, t := range mail.BuildThreads(msgs) {
		if t.RootID() == id {
			tt := t
			thread = &tt
			break
		}
	}
	// Single-message threads (or a stale/lost thread) render exactly like a
	// plain message; {id} already names that message.
	if thread == nil || thread.Count == 1 {
		s.handleMessage(w, r)
		return
	}
	// Mark the latest message read on open (matches single-message behavior),
	// honoring the mark-read delay the same way as handleMessage.
	prefs := s.store.GetPrefs()
	latest := thread.LatestMessage()
	// Under the Unread quick filter the client asks us not to auto-read on open
	// (X-SM-No-Auto-Read); marking becomes fully manual there.
	noAutoRead := r.Header.Get("X-SM-No-Auto-Read") == "1"
	wasUnread := !latest.IsRead && !noAutoRead
	if wasUnread && prefs.MarkReadDelay == 0 {
		_ = s.store.SetRead(latest.ID, true)
		_ = s.store.RecountUnread(latest.FolderID)
		msgCopy := latest
		s.background(func(ctx context.Context) error { return s.mail.SetRead(ctx, &msgCopy, true) })
	}
	qaBar, qaMenu := s.quickActionLists(prefs.QuickActions, nil)
	translateOn := s.store.GetAppConfig().TranslateAPIKey != ""
	folders, _ := s.store.ListFolders(latest.Account)
	s.renderPartial(w, "thread_detail", map[string]any{
		"CSRF":             auth.EnsureCSRF(w, r, s.secure),
		"Thread":           thread,
		"Latest":           latest,
		"Folders":          folders,
		"CurrentFolder":    latest.FolderID,
		"MarkReadDelay":    prefs.MarkReadDelay,
		"MarkReadPending":  wasUnread && prefs.MarkReadDelay > 0,
		"TranslateEnabled": translateOn,
		"DarkDefault":      prefs.DarkMessages,
		"RememberTheme":    prefs.RememberMsgTheme,
		"QABar":            qaBar,
		"QAMenu":           qaMenu,
		"QAMsg":            threadMsgActions(qaBar, qaMenu, translateOn),
	})
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	// Opening marks the message read. With a mark-read delay configured, the
	// client schedules the read after N seconds instead (see app.js); at delay 0
	// we mark it now (locally + on the server in the background). Under the Unread
	// quick filter the client sends X-SM-No-Auto-Read so opening never marks read
	// (neither now nor via MarkReadPending) — marking is fully manual there.
	prefs := s.store.GetPrefs()
	wasUnread := !msg.IsRead && r.Header.Get("X-SM-No-Auto-Read") != "1"
	if wasUnread && prefs.MarkReadDelay == 0 {
		_ = s.store.SetRead(msg.ID, true)
		_ = s.store.RecountUnread(msg.FolderID)
		msg.IsRead = true
		s.background(func(ctx context.Context) error { return s.mail.SetRead(ctx, msg, true) })
	}
	allow := false
	if msg.FromAddress != "" {
		allow, _ = s.store.SenderAllowsExternal(msg.FromAddress)
	}
	_, blocked, err := s.mail.Body(r.Context(), msg, allow, false)
	unsub, _ := s.mail.UnsubscribeInfo(r.Context(), msg)
	qaBar, qaMenu := s.quickActionLists(prefs.QuickActions, nil)
	folders, _ := s.store.ListFolders(msg.Account)
	s.renderPartial(w, "message_detail", map[string]any{
		"CSRF":             auth.EnsureCSRF(w, r, s.secure),
		"Msg":              msg,
		"Blocked":          blocked && !allow,
		"BodyErr":          err != nil,
		"Folders":          folders,
		"CurrentFolder":    msg.FolderID,
		"MarkReadDelay":    prefs.MarkReadDelay,
		"MarkReadPending":  wasUnread && prefs.MarkReadDelay > 0,
		"TranslateEnabled": s.store.GetAppConfig().TranslateAPIKey != "",
		"DarkDefault":      prefs.DarkMessages,
		"RememberTheme":    prefs.RememberMsgTheme,
		"QABar":            qaBar,
		"QAMenu":           qaMenu,
		"Unsub":            unsub,
	})
}

func (s *Server) handleMessageBody(w http.ResponseWriter, r *http.Request) {
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	allow := r.URL.Query().Get("ext") == "1"
	if !allow && msg.FromAddress != "" {
		allow, _ = s.store.SenderAllowsExternal(msg.FromAddress)
	}
	// Locked-down rendering surface for untrusted email HTML. External image
	// hosts are only permitted once the user opts in for this render.
	imgSrc := "data:"
	if allow {
		imgSrc = "data: https: http:"
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src "+imgSrc+"; style-src 'unsafe-inline'; font-src data:")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	force := r.URL.Query().Get("refresh") == "1"
	body, _, err := s.mail.Body(r.Context(), msg, allow, force)
	if err != nil {
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><body style="font:14px system-ui;color:#a1a1aa;padding:12px">Could not load this message. The account may be offline.</body>`))
		return
	}
	// #nosec G705 -- body is sanitized by the two-pass sanitizer and served under a strict CSP inside a sandboxed iframe.
	_, _ = w.Write([]byte(body))
}

func (s *Server) handleMarkRead(read bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		msg := s.messageFromReq(w, r)
		if msg == nil {
			return
		}
		// Thread-level ops (double-click on a thread row, the r/u keys, "mark
		// thread read/unread") signal X-SM-Thread and must persist to EVERY
		// message in the thread, not just the row's latest — otherwise a refresh
		// shows the thread partially unread. The per-message toggle inside a
		// thread omits the header and marks only that one message.
		if r.Header.Get("X-SM-Thread") == "1" {
			s.markThreadRead(msg, read)
		} else {
			_ = s.store.SetRead(msg.ID, read)
			_ = s.store.RecountUnread(msg.FolderID)
			msg.IsRead = read
			s.background(func(ctx context.Context) error { return s.mail.SetRead(ctx, msg, read) })
		}
		s.renderPartial(w, "message_row", msg)
	}
}

// markThreadRead persists a read/unread state to every message in msg's thread
// (the same grouping the list renders), firing a background IMAP mark for each
// and recounting every affected folder's unread total. Threads are rebuilt from
// the store at request time — threading is derived from headers, not stored —
// so this stays in sync with whatever the list actually shows.
func (s *Server) markThreadRead(msg *store.Message, read bool) {
	thread := s.threadFor(msg)
	if len(thread) == 0 {
		thread = []store.Message{*msg}
	}
	folders := map[int64]bool{}
	for _, m := range thread {
		_ = s.store.SetRead(m.ID, read)
		folders[m.FolderID] = true
		mc := m
		s.background(func(ctx context.Context) error { return s.mail.SetRead(ctx, &mc, read) })
	}
	for fid := range folders {
		_ = s.store.RecountUnread(fid)
	}
}

// threadFor returns every message grouped with msg by the list's threading
// (mail.BuildThreads), or nil when msg is a single-message thread. Scope mirrors
// the list the row came from: the unified inbox when msg sits in an inbox folder
// (unified threads can span accounts), otherwise msg's own folder.
func (s *Server) threadFor(msg *store.Message) []store.Message {
	var msgs []store.Message
	if f, _ := s.store.FolderByID(msg.FolderID); f != nil && f.SpecialUse == "inbox" {
		msgs, _ = s.store.ListUnifiedInbox(listLimit)
	} else {
		msgs, _ = s.store.ListMessages(msg.FolderID, listLimit)
	}
	for _, t := range mail.BuildThreads(msgs) {
		for _, m := range t.Messages {
			if m.ID == msg.ID {
				return t.Messages
			}
		}
	}
	return nil
}

func (s *Server) handleStar(star bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		msg := s.messageFromReq(w, r)
		if msg == nil {
			return
		}
		_ = s.store.SetStarred(msg.ID, star)
		msg.IsStarred = star
		s.background(func(ctx context.Context) error { return s.mail.SetStarred(ctx, msg, star) })
		s.renderPartial(w, "message_row", msg)
	}
}

// undoGrace is the window during which a move/archive/delete/spam action can
// be undone: the local row is relocated immediately (optimistic UI), but the
// real IMAP move is deferred until the grace period elapses so undo can just
// cancel it — no need to reconcile a remote UID that a real move would have
// reassigned.
const undoGrace = 11 * time.Second

func (s *Server) handleMove(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		msg := s.messageFromReq(w, r)
		if msg == nil {
			return
		}
		tf, err := s.store.FolderBySpecial(msg.Account, target)
		if err != nil || tf == nil {
			http.Error(w, "no "+target+" folder for this account", http.StatusBadRequest)
			return
		}
		srcFolderID := msg.FolderID
		_ = s.store.SetMessageFolder(msg.ID, tf.ID)
		_ = s.store.RecountUnread(srcFolderID)
		_ = s.store.RecountUnread(tf.ID)
		msgCopy := *msg
		s.schedulePendingMove(msgCopy.ID, func() {
			s.background(func(ctx context.Context) error { return s.mail.Move(ctx, &msgCopy, target) })
		})
		// Empty body + outerHTML swap removes the row from the list.
		w.WriteHeader(http.StatusOK)
	}
}

// handleMoveToFolder moves a message to an arbitrary folder chosen by the user
// (form value "folder" = target folder id). It generalizes handleMove's
// optimistic-move + deferred-IMAP flow to any folder, but only within the
// message's OWN account — a folder id from another account is rejected.
func (s *Server) handleMoveToFolder(w http.ResponseWriter, r *http.Request) {
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	fid, err := strconv.ParseInt(r.FormValue("folder"), 10, 64)
	if err != nil {
		http.Error(w, "missing folder", http.StatusBadRequest)
		return
	}
	tf, err := s.store.FolderByID(fid)
	if err != nil || tf == nil {
		http.Error(w, "no such folder", http.StatusBadRequest)
		return
	}
	if tf.Account != msg.Account {
		http.Error(w, "folder belongs to a different account", http.StatusBadRequest)
		return
	}
	srcFolderID := msg.FolderID
	if tf.ID == srcFolderID {
		w.WriteHeader(http.StatusOK)
		return
	}
	_ = s.store.SetMessageFolder(msg.ID, tf.ID)
	_ = s.store.RecountUnread(srcFolderID)
	_ = s.store.RecountUnread(tf.ID)
	msgCopy := *msg
	target := tf.Name
	s.schedulePendingMove(msgCopy.ID, func() {
		s.background(func(ctx context.Context) error { return s.mail.MoveToFolder(ctx, &msgCopy, target) })
	})
	// Empty body + client-side row removal (like archive/spam/delete).
	w.WriteHeader(http.StatusOK)
}

// handleUndoMove reverses a still-pending move: it cancels the deferred real
// IMAP move and puts the local row back in its original folder. folder must be
// the message's folder id before the move (the client embeds it in the toast
// at render time). Once the grace period has elapsed the move already hit the
// server, so undo is no longer possible and this reports 409.
func (s *Server) handleUndoMove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	origFolderID, err := strconv.ParseInt(r.FormValue("folder"), 10, 64)
	if err != nil {
		http.Error(w, "missing folder", http.StatusBadRequest)
		return
	}
	if !s.cancelPendingMove(id) {
		http.Error(w, "too late to undo", http.StatusConflict)
		return
	}
	msg, err := s.store.MessageByID(id)
	if err != nil || msg == nil {
		http.NotFound(w, r)
		return
	}
	_ = s.store.SetMessageFolder(id, origFolderID)
	_ = s.store.RecountUnread(msg.FolderID)
	_ = s.store.RecountUnread(origFolderID)
	w.WriteHeader(http.StatusOK)
}

// schedulePendingMove runs fn after undoGrace unless cancelPendingMove(id) is
// called first.
func (s *Server) schedulePendingMove(id int64, fn func()) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if t, ok := s.pending[id]; ok {
		t.Stop()
	}
	s.pending[id] = time.AfterFunc(undoGrace, func() {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
		fn()
	})
}

// cancelPendingMove stops a still-pending move, reporting whether it was in
// time (false once the timer already fired or there was nothing pending).
func (s *Server) cancelPendingMove(id int64) bool {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	t, ok := s.pending[id]
	if !ok {
		return false
	}
	delete(s.pending, id)
	return t.Stop()
}

// handleAllowSender persists the "always load external content" preference; the
// client reloads the iframe afterwards.
func (s *Server) handleAllowSender(w http.ResponseWriter, r *http.Request) {
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	if msg.FromAddress != "" {
		_ = s.store.SetSenderAllowsExternal(msg.FromAddress, true)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUnsubscribe performs the server-side half of a one-click/mailto
// unsubscribe (link-only unsubscribes are opened client-side and never post
// here) and reports the result as a toast over the existing SSE channel.
func (s *Server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	info, err := s.mail.UnsubscribeInfo(r.Context(), msg)
	if err != nil || info.Kind == mail.UnsubNone || info.Kind == mail.UnsubLink {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.mail.Unsubscribe(r.Context(), msg, info); err != nil {
		s.mail.Toast("Unsubscribe failed: " + err.Error())
	} else {
		s.mail.Toast("Unsubscribed")
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

// messageFromReq loads the message named by the {id} path param, writing a 404
// when absent.
func (s *Server) messageFromReq(w http.ResponseWriter, r *http.Request) *store.Message {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	msg, err := s.store.MessageByID(id)
	if err != nil || msg == nil {
		http.NotFound(w, r)
		return nil
	}
	return msg
}

// background runs an IMAP side effect off the request path so the optimistic UI
// stays snappy; failures surface as a toast over SSE.
func (s *Server) background(fn func(context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := fn(ctx); err != nil {
			s.mail.Toast(err.Error())
		}
	}()
}

func sseData(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
}

// folderNode is one node in the "Move to…" picker tree: a folder-name path
// segment that may itself be a real, selectable folder (Folder != nil) and/or
// the parent of deeper folders (Children).
type folderNode struct {
	Label    string
	Folder   *store.Folder
	Children []*folderNode
}

// folderTree turns a flat, single-account folder list into a nested tree by
// splitting each name on the IMAP hierarchy delimiter ('/' or '.', matching
// folderLabel). Special-use folders (Inbox, Sent, …) stay as flat top-level
// leaves. Intermediate segments with no folder of their own become
// non-selectable branches. Input order is preserved (folders arrive sorted).
func folderTree(folders []store.Folder) []*folderNode {
	var roots []*folderNode
	find := func(nodes *[]*folderNode, label string) *folderNode {
		for _, n := range *nodes {
			if n.Label == label {
				return n
			}
		}
		n := &folderNode{Label: label}
		*nodes = append(*nodes, n)
		return n
	}
	for i := range folders {
		f := folders[i]
		var segs []string
		if f.SpecialUse != "" {
			segs = []string{folderLabel(f)}
		} else {
			segs = strings.FieldsFunc(f.Name, func(r rune) bool { return r == '/' || r == '.' })
		}
		if len(segs) == 0 {
			segs = []string{f.Name}
		}
		cur := &roots
		var node *folderNode
		for _, s := range segs {
			node = find(cur, s)
			cur = &node.Children
		}
		fc := f
		node.Folder = &fc
	}
	return roots
}

func folderLabel(f store.Folder) string {
	if f.SpecialUse != "" {
		return strings.ToUpper(f.SpecialUse[:1]) + f.SpecialUse[1:]
	}
	name := f.Name
	if i := strings.LastIndexAny(name, "/."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

func relTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	case d < 7*24*time.Hour:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	case t.Year() == time.Now().Year():
		return t.Format("Jan 2")
	default:
		return t.Format("Jan 2006")
	}
}

// untilTime is relTime's future-facing twin: a coarse "in N …" label for a
// scheduled send. The live per-second countdown for imminent sends is done
// client-side (app.js); this is the human label for anything further out.
func untilTime(t time.Time) string {
	d := time.Until(t)
	switch {
	case d < 30*time.Second:
		return "now"
	case d < time.Hour:
		return "in " + strconv.Itoa(int(d.Minutes())+1) + " min"
	case d < 24*time.Hour:
		return "in " + strconv.Itoa(int(d.Hours())) + "h"
	case d < 48*time.Hour:
		return "tomorrow"
	default:
		return "in " + strconv.Itoa(int(d.Hours()/24)) + " days"
	}
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

// outSnippet renders a short plain-text preview of an outbox body (which may be
// HTML, markdown or plain). NOTE: a crude tag-strip + whitespace-collapse,
// not a real HTML parse — good enough for a one-line queue preview.
func outSnippet(body string) string {
	s := html.UnescapeString(tagRe.ReplaceAllString(body, " "))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 100 {
		s = strings.TrimSpace(s[:100]) + "…"
	}
	return s
}
