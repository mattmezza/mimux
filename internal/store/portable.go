package store

import (
	"fmt"
	"time"

	"github.com/mattmezza/sm/internal/config"
)

// ExportVersion is the envelope version for portable config dumps. Bump when the
// shape changes so future imports can migrate older files.
const ExportVersion = 1

// TokenExport is an OAuth2 token in the portable dump (RFC3339 expiry string).
type TokenExport struct {
	Account string `json:"account"`
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	Expiry  string `json:"expiry,omitempty"`
}

// ConfigExport is the full portable config: accounts (incl credentials and
// aliases), every app_settings row (prefs + translate/AI keys + colors) and
// OAuth tokens. It excludes sessions and all mail data.
//
// SECURITY: this contains account passwords and API keys in cleartext.
type ConfigExport struct {
	Version  int               `json:"version"`
	Accounts []config.Account  `json:"accounts"`
	Settings map[string]string `json:"settings"`
	Tokens   []TokenExport     `json:"tokens,omitempty"`
}

// Export builds a ConfigExport snapshot of the current instance.
func (s *Store) Export() (ConfigExport, error) {
	accts, err := s.ListAccounts()
	if err != nil {
		return ConfigExport{}, err
	}
	settings := map[string]string{}
	rows, err := s.DB.Query(`SELECT key, value FROM app_settings`)
	if err != nil {
		return ConfigExport{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return ConfigExport{}, err
		}
		settings[k] = v
	}
	if err := rows.Err(); err != nil {
		return ConfigExport{}, err
	}
	trows, err := s.DB.Query(`SELECT account, access_token, refresh_token, expiry FROM oauth_tokens`)
	if err != nil {
		return ConfigExport{}, err
	}
	defer func() { _ = trows.Close() }()
	var tokens []TokenExport
	for trows.Next() {
		var t TokenExport
		if err := trows.Scan(&t.Account, &t.Access, &t.Refresh, &t.Expiry); err != nil {
			return ConfigExport{}, err
		}
		tokens = append(tokens, t)
	}
	return ConfigExport{Version: ExportVersion, Accounts: accts, Settings: settings, Tokens: tokens}, trows.Err()
}

// ImportSummary reports what an Import touched.
type ImportSummary struct {
	Accounts int
	Settings int
	Tokens   int
}

// Import upserts everything in a ConfigExport. Idempotent: importing a dump taken
// from this same instance changes nothing.
func (s *Store) Import(e ConfigExport) (ImportSummary, error) {
	var sum ImportSummary
	if e.Version != ExportVersion {
		return sum, fmt.Errorf("import: unsupported version %d (expected %d)", e.Version, ExportVersion)
	}
	for _, a := range e.Accounts {
		if err := config.NormalizeAccount(&a); err != nil {
			return sum, fmt.Errorf("import: %w", err)
		}
		if err := s.UpsertAccount(a); err != nil {
			return sum, err
		}
		sum.Accounts++
	}
	for k, v := range e.Settings {
		if err := s.setSetting(k, v); err != nil {
			return sum, err
		}
		sum.Settings++
	}
	for _, t := range e.Tokens {
		var expiry time.Time
		if t.Expiry != "" {
			expiry, _ = time.Parse(time.RFC3339, t.Expiry)
		}
		if err := s.SaveToken(t.Account, StoredToken{Access: t.Access, Refresh: t.Refresh, Expiry: expiry}); err != nil {
			return sum, err
		}
		sum.Tokens++
	}
	return sum, nil
}
