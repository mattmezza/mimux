package store

import (
	"database/sql"
	"time"
)

// StoredToken is a persisted OAuth2 token. It is intentionally decoupled from
// golang.org/x/oauth2 so the store layer stays dependency-free; the mail layer
// converts to/from oauth2.Token.
type StoredToken struct {
	Access  string
	Refresh string
	Expiry  time.Time // zero = unknown
}

// SaveToken upserts the OAuth2 token for an account. A blank refresh token
// (providers only return it on first consent) does not overwrite a stored one.
func (s *Store) SaveToken(account string, t StoredToken) error {
	var expiry string
	if !t.Expiry.IsZero() {
		expiry = t.Expiry.UTC().Format(time.RFC3339)
	}
	_, err := s.DB.Exec(`
		INSERT INTO oauth_tokens (account, access_token, refresh_token, expiry, updated_at)
		VALUES (?, ?, ?, ?, datetime('now'))
		ON CONFLICT(account) DO UPDATE SET
			access_token = excluded.access_token,
			refresh_token = CASE WHEN excluded.refresh_token = '' THEN oauth_tokens.refresh_token ELSE excluded.refresh_token END,
			expiry = excluded.expiry,
			updated_at = datetime('now')`,
		account, t.Access, t.Refresh, expiry)
	return err
}

// GetToken returns the stored token for an account, or nil if none.
func (s *Store) GetToken(account string) (*StoredToken, error) {
	var t StoredToken
	var expiry string
	err := s.DB.QueryRow(`SELECT access_token, refresh_token, expiry FROM oauth_tokens WHERE account = ?`, account).
		Scan(&t.Access, &t.Refresh, &expiry)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if expiry != "" {
		t.Expiry, _ = time.Parse(time.RFC3339, expiry)
	}
	return &t, nil
}
