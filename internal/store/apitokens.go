// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

// APIScopes are the permissions an API token can carry, in display order.
// A token's scope string is a space-separated subset of these IDs; anything
// else is dropped on the way in (see ValidScopes), so a typo can never be
// stored and later honoured as a grant.
var APIScopes = []struct{ ID, Label string }{
	{"mail:read", "Read mail"},
	{"mail:send", "Send mail"},
	{"mail:modify", "Change mail (read, star, move, label)"},
	{"accounts:read", "Read the account list"},
	{"webhooks:manage", "Manage webhooks"},
}

// DefaultAPIScope is what a token gets when no scope was ticked: read-only.
const DefaultAPIScope = "mail:read"

// APIToken is one personal access token. The secret itself is never stored —
// Hash is its argon2id encoding (auth.HashPassword), and the plaintext is shown
// to the user exactly once, at creation.
type APIToken struct {
	ID         int64
	Label      string
	Hash       string
	Scopes     string    // space-separated, subset of APIScopes
	CreatedAt  time.Time
	LastUsedAt time.Time // zero = never used
	ExpiresAt  time.Time // zero = never expires
	RevokedAt  time.Time // zero = live
}

// Revoked reports whether the token was explicitly revoked.
func (t APIToken) Revoked() bool { return !t.RevokedAt.IsZero() }

// Expired reports whether the token has an expiry that has passed.
func (t APIToken) Expired() bool { return !t.ExpiresAt.IsZero() && time.Now().After(t.ExpiresAt) }

// Live reports whether the token may still authenticate a request. The API
// middleware re-checks this from the database on every request, so revoking in
// Settings takes effect immediately.
func (t APIToken) Live() bool { return !t.Revoked() && !t.Expired() }

// HasScope reports whether the token carries a scope.
func (t APIToken) HasScope(scope string) bool {
	for _, s := range strings.Fields(t.Scopes) {
		if s == scope {
			return true
		}
	}
	return false
}

// ScopeList is the token's scopes as a slice, for rendering.
func (t APIToken) ScopeList() []string { return strings.Fields(t.Scopes) }

// ValidScopes filters requested scopes down to the known ones, deduplicated and
// in APIScopes order, and returns them as the stored space-separated string.
// Nothing recognised falls back to DefaultAPIScope: a token with no scope at
// all would be a credential that authenticates and can do nothing.
func ValidScopes(want []string) string {
	order := map[string]int{}
	for i, s := range APIScopes {
		order[s.ID] = i
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range want {
		w = strings.TrimSpace(w)
		if _, ok := order[w]; !ok || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	if len(out) == 0 {
		return DefaultAPIScope
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return strings.Join(out, " ")
}

const apiTokenCols = `id, label, hash, scopes, created_at, last_used_at, expires_at, revoked_at`

func scanAPIToken(sc interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	var created string
	var lastUsed, expires, revoked sql.NullString
	if err := sc.Scan(&t.ID, &t.Label, &t.Hash, &t.Scopes, &created, &lastUsed, &expires, &revoked); err != nil {
		return t, err
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339, created)
	t.LastUsedAt = parseNullTime(lastUsed)
	t.ExpiresAt = parseNullTime(expires)
	t.RevokedAt = parseNullTime(revoked)
	return t, nil
}

func parseNullTime(v sql.NullString) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, v.String)
	return t
}

// nullTime renders a time for storage: NULL when zero.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// CreateAPIToken stores a new token, setting tok.ID and tok.CreatedAt. Hash is
// the caller's — the store never sees the secret.
func (s *Store) CreateAPIToken(tok *APIToken) error {
	tok.Label = strings.TrimSpace(tok.Label)
	if tok.Label == "" {
		return errors.New("api token: label required")
	}
	if tok.Hash == "" {
		return errors.New("api token: hash required")
	}
	tok.Scopes = ValidScopes(strings.Fields(tok.Scopes))
	tok.CreatedAt = time.Now().UTC().Truncate(time.Second)
	res, err := s.DB.Exec(`INSERT INTO api_tokens (label, hash, scopes, created_at, expires_at)
		VALUES (?,?,?,?,?)`,
		tok.Label, tok.Hash, tok.Scopes, tok.CreatedAt.Format(time.RFC3339), nullTime(tok.ExpiresAt))
	if err != nil {
		return err
	}
	tok.ID, err = res.LastInsertId()
	return err
}

// ListAPITokens returns every token, newest first — revoked and expired ones
// included, so the Settings list can show what happened to them.
func (s *Store) ListAPITokens() ([]APIToken, error) {
	rows, err := s.DB.Query(`SELECT ` + apiTokenCols + ` FROM api_tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []APIToken
	for rows.Next() {
		tok, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

// ActiveAPITokens returns the tokens that can still authenticate. This is what
// the API middleware scans on a cache miss: a single-user install has a handful
// of tokens, so an argon2id verification per candidate is affordable once, and
// the result is cached by the caller.
func (s *Store) ActiveAPITokens() ([]APIToken, error) {
	rows, err := s.DB.Query(`SELECT `+apiTokenCols+` FROM api_tokens
		WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)
		ORDER BY id DESC`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []APIToken
	for rows.Next() {
		tok, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

// APITokenByID returns one token, or nil if absent. The API middleware calls it
// on every authenticated request (one primary-key read) so that a revocation or
// an expiry is honoured without any cache to invalidate.
func (s *Store) APITokenByID(id int64) (*APIToken, error) {
	tok, err := scanAPIToken(s.DB.QueryRow(`SELECT `+apiTokenCols+` FROM api_tokens WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tok, nil
}

// RevokeAPIToken kills a token. Already-revoked tokens keep their first
// revocation time.
func (s *Store) RevokeAPIToken(id int64) error {
	_, err := s.DB.Exec(`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// TouchAPIToken records that a token was used. Callers throttle this — it is a
// write on a read path.
func (s *Store) TouchAPIToken(id int64, at time.Time) error {
	_, err := s.DB.Exec(`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339), id)
	return err
}
