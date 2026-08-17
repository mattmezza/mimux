// SPDX-License-Identifier: AGPL-3.0-only
// Package auth: argon2id password hashing, session cookies, CSRF.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	SessionCookie = "mimux_session"
	CSRFCookie    = "mimux_csrf"
	CSRFField     = "csrf_token"
	SessionTTL    = 30 * 24 * time.Hour
)

// --- passwords (argon2id, RFC 9106 low-memory params) ---

func HashPassword(password string) (string, error) {
	salt := randBytes(16)
	hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=4$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want))) // #nosec G115 -- decoded hash is 32 bytes
	return subtle.ConstantTimeCompare(got, want) == 1
}

// --- tokens ---

func NewToken() string { return base64.RawURLEncoding.EncodeToString(randBytes(32)) }

// APITokenPrefix marks a mimux personal access token (Settings → API). It is
// deliberately public and fixed: the API can reject a bearer credential that
// isn't ours before spending an argon2id verification on it, and secret
// scanners can recognise one that leaks.
const APITokenPrefix = "mimux_pat_"

// NewAPIToken mints an API token secret: the prefix plus 32 random bytes in
// base62 (alphanumeric, so it survives being pasted anywhere). Only
// HashPassword(secret) is ever stored — VerifyPassword checks a presented one.
func NewAPIToken() string {
	return APITokenPrefix + new(big.Int).SetBytes(randBytes(32)).Text(62)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	return b
}

// --- cookies ---

// Secure is set when base_url is https; plain http (LAN self-hosting) still works.
func SetSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/",
		MaxAge: int(SessionTTL.Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}

// --- CSRF (double-submit cookie) ---

// EnsureCSRF returns the request's CSRF token, minting the cookie if missing.
func EnsureCSRF(w http.ResponseWriter, r *http.Request, secure bool) string {
	if c, err := r.Cookie(CSRFCookie); err == nil && c.Value != "" {
		return c.Value
	}
	token := NewToken()
	http.SetCookie(w, &http.Cookie{
		Name: CSRFCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
	return token
}

// CSRF rejects mutating requests whose form/header token doesn't match the cookie.
func CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(CSRFCookie)
		token := r.PostFormValue(CSRFField)
		if token == "" {
			token = r.Header.Get("X-CSRF-Token") // htmx requests
		}
		if err != nil || c.Value == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) != 1 {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
