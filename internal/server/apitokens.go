// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/store"
)

// renderAPITokens re-renders the token manager. secret is the plaintext of a
// just-created token — the only moment it exists outside the user's clipboard,
// since only its hash is stored.
func (s *Server) renderAPITokens(w http.ResponseWriter, r *http.Request, secret string) {
	toks, err := s.store.ListAPITokens()
	if err != nil {
		slog.Error("api tokens: list", "err", err)
		http.Error(w, "Couldn't load API tokens.", http.StatusInternalServerError)
		return
	}
	s.renderPartial(w, "api_tokens", map[string]any{
		"CSRF":      auth.EnsureCSRF(w, r, s.secure),
		"Tokens":    toks,
		"Scopes":    store.APIScopes,
		"NewSecret": secret,
	})
}

func (s *Server) handleAPITokensManager(w http.ResponseWriter, r *http.Request) {
	s.renderAPITokens(w, r, "")
}

// handleAPITokenCreate mints a token and renders it back once. Posts via htmx
// like the other sub-managers (the settings page is one big form).
func (s *Server) handleAPITokenCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(r.PostFormValue("label"))
	if label == "" {
		http.Error(w, "Give the token a label.", http.StatusBadRequest)
		return
	}
	expires, err := parseTokenExpiry(r.PostFormValue("expires"))
	if err != nil {
		http.Error(w, "That expiry date isn't valid.", http.StatusBadRequest)
		return
	}
	secret := auth.NewAPIToken()
	hash, err := auth.HashPassword(secret)
	if err != nil {
		slog.Error("api token: hash", "err", err)
		http.Error(w, "Couldn't create the token.", http.StatusInternalServerError)
		return
	}
	tok := &store.APIToken{
		Label:     label,
		Hash:      hash,
		Scopes:    store.ValidScopes(r.PostForm["scopes"]),
		ExpiresAt: expires,
	}
	if err := s.store.CreateAPIToken(tok); err != nil {
		slog.Error("api token: create", "err", err) // never the secret or the hash
		http.Error(w, "Couldn't create the token.", http.StatusInternalServerError)
		return
	}
	s.renderAPITokens(w, r, secret)
}

func (s *Server) handleAPITokenRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.RevokeAPIToken(id); err != nil {
		slog.Error("api token: revoke", "err", err)
		http.Error(w, "Couldn't revoke the token.", http.StatusInternalServerError)
		return
	}
	s.mail.Toast("API token revoked")
	s.renderAPITokens(w, r, "")
}

// parseTokenExpiry reads the <input type="date"> value. Blank means "never".
// The token dies at the end of the chosen day, UTC — a date picked as "valid
// through Friday" should not stop working at midnight of Friday morning.
func parseTokenExpiry(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	d, err := time.ParseInLocation("2006-01-02", v, time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	return d.Add(24*time.Hour - time.Second), nil
}
