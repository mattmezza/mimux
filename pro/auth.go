//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/store"
)

// touchInterval is how often a token's last_used_at is actually written. It is
// a write on a read path, and "when was this token last used" does not need
// second precision.
const touchInterval = time.Minute

type ctxKey int

const tokenKey ctxKey = 0

// tokenOf returns the token that authenticated the request. Only ever called
// downstream of tokenAuth.require, which puts it there.
func tokenOf(r *http.Request) *store.APIToken {
	tok, _ := r.Context().Value(tokenKey).(*store.APIToken)
	return tok
}

// tokenAuth authenticates API requests against the personal access tokens in
// Settings → API, and rate-limits per token.
//
// Verification is argon2id, which is far too slow to run on every request, so
// the first success caches SHA-256(secret) -> token id. A cache hit still reads
// the row back by id (one primary-key lookup) and re-checks revoked/expired
// state, which is why nothing has to invalidate the cache when a token is
// revoked in Settings.
type tokenAuth struct {
	store *store.Store
	limit int // requests per token per minute; 0 = unlimited

	mu      sync.Mutex
	cache   []cacheEntry
	buckets map[int64]*bucket
	touched map[int64]time.Time
}

// cacheEntry is a verified secret's digest and the token it belongs to. A slice
// rather than a map: the lookup compares digests in constant time, and a
// single-user install has a handful of tokens.
type cacheEntry struct {
	sum [32]byte
	id  int64
}

func newTokenAuth(st *store.Store, limit int) *tokenAuth {
	return &tokenAuth{
		store:   st,
		limit:   limit,
		buckets: map[int64]*bucket{},
		touched: map[int64]time.Time{},
	}
}

// require is the chi middleware: valid bearer token, within its rate limit, or
// the request never reaches a handler.
func (a *tokenAuth) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := bearer(r)
		if !ok {
			apiError(w, http.StatusUnauthorized, "unauthorized", "Missing or malformed Authorization header. Send: Authorization: Bearer <token>.")
			return
		}
		tok, err := a.lookup(secret)
		if err != nil {
			slog.Error("api auth", "err", err) // never the token or its hash
			apiError(w, http.StatusInternalServerError, "internal", "Couldn't verify the token.")
			return
		}
		if tok == nil {
			apiError(w, http.StatusUnauthorized, "unauthorized", "Unknown, revoked or expired token.")
			return
		}
		if retry, ok := a.allow(tok.ID); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			apiError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests for this token. Retry in "+strconv.Itoa(retry)+"s.")
			return
		}
		a.touch(tok.ID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey, tok)))
	})
}

// requireScope rejects a token that does not carry the scope, naming it so the
// caller can go and tick the right box in Settings.
func requireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := tokenOf(r)
			if tok == nil || !tok.HasScope(scope) {
				apiError(w, http.StatusForbidden, "insufficient_scope", "This token is missing the "+scope+" scope.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearer pulls the token out of the Authorization header. The prefix check is
// what keeps an unrelated bearer credential from costing an argon2id pass.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, value, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, auth.APITokenPrefix) {
		return "", false
	}
	return value, true
}

// lookup resolves a presented secret to its live token, or nil if there isn't
// one. Cache hit: one indexed read plus a constant-time digest comparison.
// Cache miss: argon2id against each active token, then remember the digest.
func (a *tokenAuth) lookup(secret string) (*store.APIToken, error) {
	sum := sha256.Sum256([]byte(secret))
	if id, ok := a.cached(sum); ok {
		tok, err := a.store.APITokenByID(id)
		if err != nil || tok == nil || !tok.Live() {
			return nil, err
		}
		return tok, nil
	}
	active, err := a.store.ActiveAPITokens()
	if err != nil {
		return nil, err
	}
	for i := range active {
		if !auth.VerifyPassword(secret, active[i].Hash) {
			continue
		}
		a.mu.Lock()
		a.cache = append(a.cache, cacheEntry{sum: sum, id: active[i].ID})
		a.mu.Unlock()
		return &active[i], nil
	}
	return nil, nil
}

// cached scans the verified-digest cache in constant time per entry, so a
// near-miss cannot be told from a far-miss by how long the comparison took.
func (a *tokenAuth) cached(sum [32]byte) (int64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var id int64
	var found bool
	for _, e := range a.cache {
		if subtle.ConstantTimeCompare(e.sum[:], sum[:]) == 1 {
			id, found = e.id, true
		}
	}
	return id, found
}

// touch records the token's use, at most once a minute per token.
func (a *tokenAuth) touch(id int64) {
	now := time.Now()
	a.mu.Lock()
	if last, ok := a.touched[id]; ok && now.Sub(last) < touchInterval {
		a.mu.Unlock()
		return
	}
	a.touched[id] = now
	a.mu.Unlock()
	if err := a.store.TouchAPIToken(id, now); err != nil {
		slog.Warn("api auth: last-used", "token", id, "err", err)
	}
}

// bucket is a per-token token bucket: `tokens` refills continuously up to the
// per-minute limit.
type bucket struct {
	tokens float64
	last   time.Time
}

// allow spends one request from the token's bucket. On refusal it returns the
// whole seconds to wait for the next one, for Retry-After.
func (a *tokenAuth) allow(id int64) (int, bool) {
	if a.limit <= 0 {
		return 0, true
	}
	limit := float64(a.limit)
	perSec := limit / 60
	now := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.buckets[id]
	if !ok {
		b = &bucket{tokens: limit, last: now}
		a.buckets[id] = b
	}
	b.tokens = math.Min(limit, b.tokens+now.Sub(b.last).Seconds()*perSec)
	b.last = now
	if b.tokens < 1 {
		return int(math.Ceil((1 - b.tokens) / perSec)), false
	}
	b.tokens--
	return 0, true
}

// apiError writes the API's error envelope. Every non-2xx answer from the API
// has this shape, so a client can parse failures without special-casing.
func apiError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
