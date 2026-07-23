package server

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
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
	"folderLabel":    folderLabel,
	"messageLabels":  mail.MessageLabels,
	"dict":           dict,
	"highlight":      highlight,
	"identities":     identities,
	"receivedAlias":  receivedAlias,
	"hasQA":          hasQA,
	"shortURL":       shortURL,
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

// hasQA reports whether action id is enabled in the comma-separated
// QuickActions preference string.
func hasQA(id, quickActions string) bool {
	for _, a := range strings.Split(quickActions, ",") {
		if a == id {
			return true
		}
	}
	return false
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
	wasUnread := !latest.IsRead
	if wasUnread && prefs.MarkReadDelay == 0 {
		_ = s.store.SetRead(latest.ID, true)
		_ = s.store.RecountUnread(latest.FolderID)
		msgCopy := latest
		s.background(func(ctx context.Context) error { return s.mail.SetRead(ctx, &msgCopy, true) })
	}
	s.renderPartial(w, "thread_detail", map[string]any{
		"CSRF":             auth.EnsureCSRF(w, r, s.secure),
		"Thread":           thread,
		"Latest":           latest,
		"MarkReadDelay":    prefs.MarkReadDelay,
		"MarkReadPending":  wasUnread && prefs.MarkReadDelay > 0,
		"TranslateEnabled": s.cfg.Translate.APIKey != "",
		"DarkDefault":      prefs.DarkMessages,
		"RememberTheme":    prefs.RememberMsgTheme,
		"QuickActions":     prefs.QuickActions,
	})
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	// Opening marks the message read. With a mark-read delay configured, the
	// client schedules the read after N seconds instead (see app.js); at delay 0
	// we mark it now (locally + on the server in the background).
	prefs := s.store.GetPrefs()
	wasUnread := !msg.IsRead
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
	s.renderPartial(w, "message_detail", map[string]any{
		"CSRF":             auth.EnsureCSRF(w, r, s.secure),
		"Msg":              msg,
		"Blocked":          blocked && !allow,
		"BodyErr":          err != nil,
		"CurrentFolder":    msg.FolderID,
		"MarkReadDelay":    prefs.MarkReadDelay,
		"MarkReadPending":  wasUnread && prefs.MarkReadDelay > 0,
		"TranslateEnabled": s.cfg.Translate.APIKey != "",
		"DarkDefault":      prefs.DarkMessages,
		"RememberTheme":    prefs.RememberMsgTheme,
		"QuickActions":     prefs.QuickActions,
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
		_ = s.store.SetRead(msg.ID, read)
		_ = s.store.RecountUnread(msg.FolderID)
		msg.IsRead = read
		s.background(func(ctx context.Context) error { return s.mail.SetRead(ctx, msg, read) })
		s.renderPartial(w, "message_row", msg)
	}
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
