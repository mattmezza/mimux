//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/ext"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// api is the /api/v1 JSON surface: the same domain layer the HTML client uses
// (ext.Deps), driven by machines instead of a browser.
type api struct {
	mail  *mail.Manager
	store *store.Store
	deps  ext.Deps
	jobs  *jobTable
	idem  *idemCache
}

func newAPI(deps ext.Deps) *api {
	return &api{mail: deps.Mail, store: deps.Store, deps: deps, jobs: newJobTable(), idem: newIdemCache()}
}

// mount registers every v1 route on r, which is already behind tokens.require.
// Scope rule: reading mail needs mail:read, sending/drafting mail:send, every
// mutation mail:modify, account status accounts:read. Filters are read under
// mail:read and written under mail:modify — they govern how mail is handled,
// so they follow the mail scopes rather than getting one of their own.
func (a *api) mount(r chi.Router) {
	r.With(requireScope("accounts:read")).Get("/accounts", a.handleAccounts)

	r.Group(func(r chi.Router) {
		r.Use(requireScope("mail:read"))
		r.Get("/folders", a.handleFolders)
		r.Get("/messages", a.handleListMessages)
		r.Get("/messages/{id}", a.handleGetMessage)
		r.Get("/messages/{id}/attachments/{n}", a.handleAttachment)
		r.Post("/messages/search", a.handleSearch)
		r.Get("/search/jobs/{id}", a.handleSearchJob)
		r.Get("/filters", a.handleListFilters)
	})

	r.Group(func(r chi.Router) {
		r.Use(requireScope("mail:send"))
		r.Post("/messages/send", a.idem.wrap(a.handleSend))
		r.Post("/drafts", a.idem.wrap(a.handleCreateDraft))
		r.Patch("/drafts/{id}", a.handleUpdateDraft)
	})

	r.Group(func(r chi.Router) {
		r.Use(requireScope("mail:modify"))
		r.Patch("/messages/{id}", a.handlePatchMessage)
		r.Post("/messages/{id}/archive", a.handleMove("archive"))
		r.Post("/messages/{id}/spam", a.handleMove("spam"))
		r.Delete("/messages/{id}", a.handleMove("trash"))
		r.Post("/filters", a.idem.wrap(a.handleCreateFilter))
	})
}

// --- shared helpers ---

// decodeJSON parses a JSON request body into v, rejecting unknown fields so a
// typo'd field name fails loudly instead of being silently ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "Bad JSON body: "+err.Error())
		return false
	}
	return true
}

// pathID parses the {id} path param, writing the 404 envelope when malformed.
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "No such resource.")
		return 0, false
	}
	return id, true
}

// messageOr404 loads a message by the {id} path param.
func (a *api) messageOr404(w http.ResponseWriter, r *http.Request) *store.Message {
	id, ok := pathID(w, r)
	if !ok {
		return nil
	}
	msg, err := a.store.MessageByID(id)
	if err != nil || msg == nil {
		apiError(w, http.StatusNotFound, "not_found", "No message with id "+strconv.FormatInt(id, 10)+".")
		return nil
	}
	return msg
}

// background runs an IMAP side effect off the request path, mirroring the HTML
// handlers' optimistic pattern: the store is already updated, the server write
// follows. Failures are logged (no browser to toast at); flag writes are also
// retried by the sync loop's seen-dirty sweep.
func (a *api) background(op string, fn func(context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := fn(ctx); err != nil {
			slog.Warn("api: background "+op, "err", err)
		}
	}()
}

// writeJSONStatus is writeJSON with a non-200 status (headers must be set
// before WriteHeader, so plain writeJSON can't follow one).
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// list is the collection envelope: {"data": [...], "next_cursor": "..."}.
func writeList(w http.ResponseWriter, data any, next string) {
	writeJSON(w, map[string]any{"data": data, "next_cursor": next})
}

// --- message JSON shape ---

type addrJSON struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

type messageJSON struct {
	ID            int64     `json:"id"`
	Account       string    `json:"account"`
	FolderID      int64     `json:"folder_id"`
	MessageID     string    `json:"message_id,omitempty"`
	InReplyTo     string    `json:"in_reply_to,omitempty"`
	From          addrJSON  `json:"from"`
	To            []string  `json:"to"`
	Cc            []string  `json:"cc,omitempty"`
	Subject       string    `json:"subject"`
	Date          time.Time `json:"date"`
	Size          int64     `json:"size"`
	IsRead        bool      `json:"is_read"`
	IsStarred     bool      `json:"is_starred"`
	HasAttachment bool      `json:"has_attachment"`
	Snippet       string    `json:"snippet"`
	Labels        []string  `json:"labels,omitempty"`
	ThreadID      int64     `json:"thread_id,omitempty"`
	ThreadSize    int       `json:"thread_size,omitempty"`
}

func toMessageJSON(m store.Message) messageJSON {
	return messageJSON{
		ID:            m.ID,
		Account:       m.Account,
		FolderID:      m.FolderID,
		MessageID:     m.MessageID,
		InReplyTo:     m.InReplyTo,
		From:          addrJSON{Name: m.FromName, Address: m.FromAddress},
		To:            mail.SplitAddrList(m.ToAddresses),
		Cc:            mail.SplitAddrList(m.CcAddresses),
		Subject:       m.Subject,
		Date:          m.Date.UTC(),
		Size:          m.Size,
		IsRead:        m.IsRead,
		IsStarred:     m.IsStarred,
		HasAttachment: m.HasAttachment,
		Snippet:       m.Snippet,
		Labels:        mail.MessageLabels(m.Labels),
	}
}

// --- idempotency ---

// idemCache replays the stored response of a completed mutating POST when the
// same Idempotency-Key comes back, so a retried send can't go out twice. Only
// 2xx responses are stored: a failed call is safe to re-execute.
//
// ponytail: in-memory, so keys don't survive a restart (documented in the API
// docs as at-most-once per process). Persist to SQLite if that ever bites.
type idemCache struct {
	mu sync.Mutex
	m  map[string]idemEntry
}

type idemEntry struct {
	status int
	body   []byte
	at     time.Time
}

const idemTTL = 24 * time.Hour
const idemMax = 1024

func newIdemCache() *idemCache { return &idemCache{m: map[string]idemEntry{}} }

func (c *idemCache) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next(w, r)
			return
		}
		// Scope the key to the token and route, so two clients (or two
		// endpoints) can't collide on a generic key like "1".
		full := strconv.FormatInt(tokenOf(r).ID, 10) + "\x00" + r.Method + " " + r.URL.Path + "\x00" + key
		c.mu.Lock()
		e, ok := c.m[full]
		c.mu.Unlock()
		if ok && time.Since(e.at) < idemTTL {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotency-Replayed", "true")
			w.WriteHeader(e.status)
			_, _ = w.Write(e.body)
			return
		}
		rec := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)
		if rec.status >= 200 && rec.status < 300 {
			c.mu.Lock()
			if len(c.m) >= idemMax {
				// ponytail: full cache drops ALL entries rather than tracking
				// LRU order — 1024 mutating calls per day per install is far
				// beyond real use; revisit if that assumption breaks.
				c.m = map[string]idemEntry{}
			}
			c.m[full] = idemEntry{status: rec.status, body: rec.body, at: time.Now()}
			c.mu.Unlock()
		}
	}
}

// captureWriter tees the response so the idempotency cache can replay it.
type captureWriter struct {
	http.ResponseWriter
	status int
	body   []byte
	wrote  bool
}

func (c *captureWriter) WriteHeader(status int) {
	if !c.wrote {
		c.status = status
		c.wrote = true
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	c.body = append(c.body, b...)
	return c.ResponseWriter.Write(b)
}
