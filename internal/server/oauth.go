// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

const oauthStateCookie = "mimux_oauth_state"

// handleOAuthStart redirects to the provider consent screen. The state param
// carries "<account>:<nonce>"; the nonce is also set in a short-lived cookie
// and re-checked on callback (CSRF protection for the OAuth flow).
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ac, ok := s.accountByName(name)
	if !ok || ac.Auth != "oauth2" {
		http.NotFound(w, r)
		return
	}
	oc, err := mail.OAuthConfig(ac, s.cfg.Server.BaseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	nonce := auth.NewToken()
	http.SetCookie(w, &http.Cookie{
		Name: oauthStateCookie, Value: nonce, Path: "/",
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	// offline + prompt=consent so the provider returns a refresh token.
	url := oc.AuthCodeURL(name+":"+nonce,
		oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
	http.Redirect(w, r, url, http.StatusFound)
}

// handleOAuthCallback exchanges the authorization code for tokens, stores them
// and wakes the account worker.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	name, nonce, ok := strings.Cut(r.URL.Query().Get("state"), ":")
	c, err := r.Cookie(oauthStateCookie)
	if !ok || err != nil || c.Value == "" || c.Value != nonce {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1})
	ac, found := s.accountByName(name)
	if !found || ac.Auth != "oauth2" {
		http.NotFound(w, r)
		return
	}
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		http.Error(w, "authorization denied: "+errMsg, http.StatusBadRequest)
		return
	}
	oc, err := mail.OAuthConfig(ac, s.cfg.Server.BaseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tok, err := oc.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		slog.Error("oauth exchange", "account", name, "err", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	if err := s.store.SaveToken(name, store.StoredToken{
		Access: tok.AccessToken, Refresh: tok.RefreshToken, Expiry: tok.Expiry,
	}); err != nil {
		slog.Error("oauth save token", "account", name, "err", err)
		http.Error(w, "could not store token", http.StatusInternalServerError)
		return
	}
	s.mail.Wake(name)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
