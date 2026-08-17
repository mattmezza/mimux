// SPDX-License-Identifier: AGPL-3.0-only
// Package server: HTTP routes, middleware, template rendering.
package server

import (
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/mattmezza/mimux/internal/ai"
	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/filter"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
	"github.com/mattmezza/mimux/web"
)

type Server struct {
	cfg     *config.Config
	store   *store.Store
	mail    *mail.Manager
	tmpl    map[string]*template.Template
	secure  bool
	version string

	pendingMu sync.Mutex
	pending   map[int64]*time.Timer // message id -> deferred real IMAP move, cancellable by undo
}

func New(cfg *config.Config, st *store.Store, mgr *mail.Manager, version string) (*Server, error) {
	s := &Server{
		cfg:     cfg,
		store:   st,
		mail:    mgr,
		secure:  strings.HasPrefix(cfg.Server.BaseURL, "https://"),
		version: version,
		pending: map[int64]*time.Timer{},
	}
	s.refreshAccounts()
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// accounts returns the current DB-backed accounts (a snapshot refreshed by
// refreshAccounts). Single accessor used everywhere the account list is read.
func (s *Server) accounts() []config.Account { return s.cfg.Accounts }

// refreshAccounts reloads the account snapshot from the DB into cfg and rebuilds
// the alias lookup. Call after any account mutation and at startup.
func (s *Server) refreshAccounts() {
	accts, err := s.store.ListAccounts()
	if err != nil {
		slog.Error("refreshAccounts", "err", err)
		return
	}
	s.cfg.Accounts = accts
	setAccountAliases(accts)
	setAccountLabels(accts)
}

// parseTemplates builds one template set per page, each including the base
// layout and all partials/components.
func (s *Server) parseTemplates() error {
	s.tmpl = map[string]*template.Template{}
	pages, err := fs.Glob(web.FS, "templates/pages/*.html")
	if err != nil {
		return err
	}
	shared, err := fs.Glob(web.FS, "templates/layouts/*.html")
	if err != nil {
		return err
	}
	for _, pat := range []string{"templates/partials/*.html", "templates/components/*.html"} {
		m, _ := fs.Glob(web.FS, pat)
		shared = append(shared, m...)
	}
	for _, page := range pages {
		name := strings.TrimSuffix(page[strings.LastIndex(page, "/")+1:], ".html")
		t, err := template.New("").Funcs(templateFuncs).ParseFS(web.FS, append([]string{page}, shared...)...)
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.tmpl[name] = t
	}
	return nil
}

// renderPartial executes a single named template (an htmx swap fragment). Every
// page set includes all partials, so the inbox set can render any of them.
func (s *Server) renderPartial(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl["inbox"].ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render partial", "name", name, "err", err)
	}
}

func (s *Server) render(w http.ResponseWriter, page string, data map[string]any) {
	t, ok := s.tmpl[page]
	if !ok {
		http.Error(w, "template not found: "+page, http.StatusInternalServerError)
		return
	}
	if data == nil {
		data = make(map[string]any)
	}
	data["Version"] = s.version
	// The spinner's starting state, so a cold load paints it right on first
	// paint instead of waiting for the first SSE event.
	data["Syncing"] = s.mail.AnySyncing()
	s.appearanceData(data)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		slog.Error("render", "page", page, "err", err)
	}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger, middleware.Recoverer)
	r.Use(auth.CSRF)

	staticFS, _ := fs.Sub(web.FS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServerFS(staticFS)))
	// Service worker must be served from the root scope.
	r.Get("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		b, _ := fs.ReadFile(web.FS, "static/js/sw.js")
		_, _ = w.Write(b)
	})
	// Manifest and icon are generated from the stored look settings (and must
	// stay reachable pre-auth: the login and setup pages show the icon too).
	r.Get("/manifest.json", s.handleManifest)
	r.Get("/icon.svg", s.handleIcon)

	// Extensions (pro build only — nothing in the free build) mount outside the
	// auth group below: they are machine-facing and bring their own auth.
	s.mountExtensions(r)

	r.Get("/setup", s.handleSetupForm)
	r.Post("/setup", s.handleSetup)
	r.Get("/login", s.handleLoginForm)
	r.Post("/login", s.handleLogin)
	r.Post("/logout", s.handleLogout)

	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/", s.handleInbox)
		r.Get("/events", s.handleEvents)
		r.Get("/search", s.handleSearch)
		r.Get("/search/suggest", s.handleSearchSuggest)
		r.Post("/search/server", s.handleServerSearch)
		r.Post("/search/history/clear", s.handleHistoryClear)
		r.Post("/search/saved", s.handleSaveSearch)
		r.Post("/search/saved/{id}/delete", s.handleDeleteSavedSearch)
		r.Get("/u", s.handleUnified)
		r.Get("/f/{id}", s.handleFolder)
		r.Get("/t/{id}", s.handleThread)
		r.Get("/t/{id}/rows", s.handleThreadRows)
		r.Get("/rowmenu/{id}", s.handleRowMenu)
		r.Get("/oauth/{name}/start", s.handleOAuthStart)
		r.Get("/oauth/callback", s.handleOAuthCallback)
		r.Get("/statusbar", s.handleStatusbar)
		r.Get("/accounts/info", s.handleAccountsInfo)
		r.Get("/health", s.handleHealth)
		r.Get("/unread", s.handleUnread)
		r.Post("/refresh", s.handleRefresh)
		r.Get("/messages/{id}", s.handleMessage)
		r.Get("/messages/{id}/body", s.handleMessageBody)
		r.Get("/messages/{id}/attachments", s.handleAttachments)
		r.Get("/messages/{id}/ext-banner", s.handleExtBanner)
		r.Get("/messages/{id}/attachment/{part}", s.handleAttachment)
		r.Get("/messages/{id}/invite", s.handleInvite)
		r.Get("/messages/{id}/summary", s.handleMessageSummary)
		r.Post("/messages/{id}/rsvp", s.handleRSVP)
		r.Post("/messages/{id}/read", s.handleMarkRead(true))
		r.Post("/messages/{id}/unread", s.handleMarkRead(false))
		r.Post("/messages/{id}/star", s.handleStar(true))
		r.Post("/messages/{id}/unstar", s.handleStar(false))
		r.Post("/messages/{id}/delete", s.handleMove("trash"))
		r.Post("/messages/{id}/archive", s.handleMove("archive"))
		r.Post("/messages/{id}/spam", s.handleMove("spam"))
		r.Post("/messages/{id}/move", s.handleMoveToFolder)
		r.Post("/messages/{id}/undo-move", s.handleUndoMove)
		r.Post("/messages/{id}/label", s.handleLabel)
		r.Post("/messages/{id}/allow-sender", s.handleAllowSender)
		r.Post("/messages/{id}/unsubscribe", s.handleUnsubscribe)
		r.Get("/compose", s.handleComposeNew)
		r.Get("/compose/suggest", s.handleAddressSuggest)
		r.Post("/compose", s.handleComposeSend)
		r.Post("/compose/draft", s.handleComposeDraftSave)
		r.Post("/compose/preview", s.handleComposePreview)
		r.Get("/drafts", s.handleDraftsPage)
		r.Post("/drafts/{id}/delete", s.handleDraftDelete)
		r.Get("/outgoing", s.handleOutgoingPage)
		r.Get("/outgoing/list", s.handleOutgoingList)
		r.Post("/outbox/{id}/undo", s.handleOutboxUndo)
		r.Post("/outbox/{id}/cancel", s.handleOutboxCancel)
		r.Post("/outbox/{id}/retry", s.handleOutboxRetry)
		r.Post("/outbox/{id}/delete", s.handleOutboxDelete)
		r.Get("/settings", s.handleSettings)
		r.Post("/settings", s.handleSettingsSave)
		r.Get("/settings/accounts", s.handleAccountsManager)
		r.Post("/settings/accounts", s.handleAccountSave)
		r.Post("/settings/accounts/{name}/delete", s.handleAccountDelete)
		r.Get("/settings/signatures", s.handleSignaturesManager)
		r.Post("/settings/signatures", s.handleSignatureSave)
		r.Post("/settings/signatures/link", s.handleSignatureLink)
		r.Post("/settings/signatures/{id}/delete", s.handleSignatureDelete)
		r.Get("/settings/api", s.handleAPITokensManager)
		r.Post("/settings/api", s.handleAPITokenCreate)
		r.Post("/settings/api/{id}/revoke", s.handleAPITokenRevoke)
		r.Get("/settings/webhooks", s.handleWebhooksManager)
		r.Post("/settings/webhooks", s.handleWebhookCreate)
		r.Post("/settings/webhooks/{id}/enable", s.handleWebhookEnable)
		r.Post("/settings/webhooks/{id}/delete", s.handleWebhookDelete)
		r.Post("/settings/webhooks/{id}/deliveries/{did}/replay", s.handleWebhookReplay)
		r.Get("/settings/licence", s.handleLicenceManager)
		r.Post("/settings/licence", s.handleLicenceSave)
		r.Post("/settings/licence/remove", s.handleLicenceRemove)
		r.Get("/settings/templates", s.handleTemplatesManager)
		r.Post("/settings/templates", s.handleTemplateSave)
		r.Post("/settings/templates/{id}/delete", s.handleTemplateDelete)
		r.Get("/push/devices", s.handlePushDevices)
		r.Post("/push/subscribe", s.handlePushSubscribe)
		r.Post("/push/unsubscribe", s.handlePushUnsubscribe)
		r.Post("/push/test", s.handleNotifyTest)
		r.Get("/settings/export", s.handleConfigExport)
		r.Post("/settings/import", s.handleConfigImport)
		r.Mount("/filters", filter.Routes(s.store, s.secure, templateFuncs, func() any { return s.sidebarData() }))
		// Clients are built per request so AI keys edited in Settings take
		// effect without a restart (translate does the same, in handleMessageBody).
		r.Mount("/ai", ai.Routes(s.aiClient))
	})
	return r
}

// requireAuth redirects to /setup (first run) or /login.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(auth.SessionCookie)
		if err == nil && c.Value != "" {
			if uid, _ := s.store.SessionUserID(c.Value); uid != 0 {
				next.ServeHTTP(w, r)
				return
			}
		}
		if n, _ := s.store.UserCount(); n == 0 {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// --- handlers ---

func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	if n, _ := s.store.UserCount(); n > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, "setup", map[string]any{"CSRF": auth.EnsureCSRF(w, r, s.secure)})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if n, _ := s.store.UserCount(); n > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	username, password := r.PostFormValue("username"), r.PostFormValue("password")
	if username == "" || len(password) < 8 {
		s.render(w, "setup", map[string]any{"CSRF": auth.EnsureCSRF(w, r, s.secure), "Error": "Username required; password must be at least 8 characters."})
		return
	}
	hash, err := auth.HashPassword(password)
	if err == nil {
		err = s.store.CreateUser(username, hash)
	}
	if err != nil {
		slog.Error("setup", "err", err)
		http.Error(w, "setup failed", http.StatusInternalServerError)
		return
	}
	s.startSession(w, r, username)
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if n, _ := s.store.UserCount(); n == 0 {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	s.render(w, "login", map[string]any{"CSRF": auth.EnsureCSRF(w, r, s.secure)})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	username, password := r.PostFormValue("username"), r.PostFormValue("password")
	u, err := s.store.UserByName(username)
	if err != nil {
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}
	if u == nil || !auth.VerifyPassword(password, u.PasswordHash) {
		s.render(w, "login", map[string]any{"CSRF": auth.EnsureCSRF(w, r, s.secure), "Error": "Wrong username or password."})
		return
	}
	s.startSession(w, r, username)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, username string) {
	u, err := s.store.UserByName(username)
	if err != nil || u == nil {
		http.Error(w, "session failed", http.StatusInternalServerError)
		return
	}
	token := auth.NewToken()
	if err := s.store.CreateSession(token, u.ID, time.Now().Add(auth.SessionTTL)); err != nil {
		http.Error(w, "session failed", http.StatusInternalServerError)
		return
	}
	auth.SetSessionCookie(w, token, s.secure)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		_ = s.store.DeleteSession(c.Value)
	}
	auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	prefs := s.store.GetPrefs()
	totalUnread, _ := s.store.TotalInboxUnread()
	data := map[string]any{
		"CSRF":          auth.EnsureCSRF(w, r, s.secure),
		"Accounts":      s.cfg.Accounts,
		"Sidebar":       s.sidebarData(),
		"Statuses":      s.mail.Status(),
		"Unified":       len(s.cfg.Accounts) > 1,
		"Saved":         s.savedSearches(),
		"Prefs":         prefs,
		"AccountColors": prefs.AccountColors,
		"TotalUnread":   totalUnread,
	}
	// Deep link to a specific folder (?f=<id>) — used by the sidebar links when
	// navigating in from another full page (filters/drafts), where there is no
	// #message-list to htmx-swap into. A bookmarked thread URL (?t=<id>&src=<n>)
	// carries the list scope in src, so a numeric src selects the same folder
	// (the client then opens the thread into the reading pane, see app.js).
	fid := r.URL.Query().Get("f")
	if fid == "" {
		if src := r.URL.Query().Get("src"); src != "" && src != "u" {
			fid = src
		}
	}
	if fid != "" {
		if id, err := strconv.ParseInt(fid, 10, 64); err == nil {
			if f, _ := s.store.FolderByID(id); f != nil {
				s.fillList(data, f, false, "")
				s.render(w, "inbox", data)
				return
			}
		}
	}
	// Default landing view: unified inbox when more than one account, the
	// single account's inbox otherwise.
	if len(s.cfg.Accounts) > 1 {
		s.fillList(data, nil, true, "")
	} else if current, _ := s.store.FirstInbox(); current != nil {
		s.fillList(data, current, false, "")
	}
	s.render(w, "inbox", data)
}

// handleEvents is the SSE stream: it relays sync-status, new-mail and toast
// events from the mail manager to the browser.
//
// sync-status carries the aggregate "is any account syncing" ("1"/"0") rather
// than the account name nobody read, recomputed at send time so it is current
// truth and not a snapshot of when the event was queued. The browser draws its
// spinner straight from it and never asks the server anything.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	events, unsubscribe := s.mail.Subscribe()
	defer unsubscribe()
	_, _ = fmt.Fprint(w, "event: hello\ndata: {}\n\n")
	// Current sync state up front, so a reconnecting browser holding a stale
	// page is corrected immediately without a separate endpoint to ask.
	sync := syncFlag(s.mail.AnySyncing())
	_, _ = fmt.Fprintf(w, "event: sync-status\ndata: %s\n\n", sync)
	f.Flush()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-events:
			data := sseData(e.Data)
			if e.Type == "sync-status" {
				data = syncFlag(s.mail.AnySyncing())
				sync = data
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data)
			f.Flush()
		case <-t.C:
			// The hub drops events for a subscriber that fell behind, and a
			// dropped "nobody is syncing" would strand the spinner on. The
			// keepalive re-states the truth when it has drifted; when it hasn't
			// (the normal case) it stays a bare comment.
			if now := syncFlag(s.mail.AnySyncing()); now != sync {
				sync = now
				_, _ = fmt.Fprintf(w, "event: sync-status\ndata: %s\n\n", now)
			} else {
				_, _ = fmt.Fprint(w, ": ping\n\n")
			}
			f.Flush()
		}
	}
}

// syncFlag is the sync-status SSE payload: the spinner is a boolean, so send a
// boolean.
func syncFlag(any bool) string {
	if any {
		return "1"
	}
	return "0"
}
