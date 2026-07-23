package store

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/mattmezza/sm/internal/config"
)

// accountCols is the column list shared by the account queries.
const accountCols = `name, sender_name, provider, email, auth, password,
	oauth2_client_id, oauth2_client_secret, imap_host, imap_port,
	smtp_host, smtp_port, aliases, position`

func scanAccount(sc interface{ Scan(...any) error }) (config.Account, int, error) {
	var a config.Account
	var aliasesJSON string
	var pos int
	err := sc.Scan(&a.Name, &a.SenderName, &a.Provider, &a.Email, &a.Auth, &a.Password,
		&a.OAuth2ClientID, &a.OAuth2ClientSecret, &a.IMAPHost, &a.IMAPPort,
		&a.SMTPHost, &a.SMTPPort, &aliasesJSON, &pos)
	if err != nil {
		return a, 0, err
	}
	if aliasesJSON != "" {
		_ = json.Unmarshal([]byte(aliasesJSON), &a.Aliases)
	}
	return a, pos, nil
}

// ListAccounts returns every account, normalized (preset hosts filled in), in
// display order. A malformed row that fails normalization is skipped rather than
// failing the whole list.
func (s *Store) ListAccounts() ([]config.Account, error) {
	rows, err := s.DB.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY position, name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []config.Account
	for rows.Next() {
		a, _, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		if err := config.NormalizeAccount(&a); err != nil {
			continue // skip a broken account instead of blanking the whole app
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount returns one account by name (normalized), or nil if absent.
func (s *Store) GetAccount(name string) (*config.Account, error) {
	row := s.DB.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE name = ?`, name)
	a, _, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := config.NormalizeAccount(&a); err != nil {
		return nil, err
	}
	return &a, nil
}

// UpsertAccount inserts or replaces an account (keyed by name). Aliases are
// stored as JSON. Provider-preset hosts are left blank in the row (filled in on
// read) so a preset change re-derives them.
func (s *Store) UpsertAccount(a config.Account) error {
	if a.Name == "" {
		return errors.New("account: name required")
	}
	aliases := a.Aliases
	if aliases == nil {
		aliases = []config.Alias{}
	}
	b, err := json.Marshal(aliases)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`
		INSERT INTO accounts (`+accountCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?, COALESCE((SELECT position FROM accounts WHERE name = ?), (SELECT COALESCE(MAX(position),0)+1 FROM accounts)))
		ON CONFLICT(name) DO UPDATE SET
			sender_name = excluded.sender_name,
			provider = excluded.provider,
			email = excluded.email,
			auth = excluded.auth,
			password = excluded.password,
			oauth2_client_id = excluded.oauth2_client_id,
			oauth2_client_secret = excluded.oauth2_client_secret,
			imap_host = excluded.imap_host,
			imap_port = excluded.imap_port,
			smtp_host = excluded.smtp_host,
			smtp_port = excluded.smtp_port,
			aliases = excluded.aliases`,
		a.Name, a.SenderName, a.Provider, a.Email, a.Auth, a.Password,
		a.OAuth2ClientID, a.OAuth2ClientSecret, a.IMAPHost, a.IMAPPort,
		a.SMTPHost, a.SMTPPort, string(b), a.Name)
	return err
}

// DeleteAccount removes an account and all of its synced mail in one
// transaction: folders (cascading to messages, bodies and the FTS index),
// filter rules, the OAuth token, and the account's color preference.
func (s *Store) DeleteAccount(name string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM folders WHERE account = ?`,
		`DELETE FROM filter_rules WHERE account = ?`,
		`DELETE FROM oauth_tokens WHERE account = ?`,
	} {
		if _, err := tx.Exec(q, name); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM app_settings WHERE key = ?`, accountColorPrefix+name); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM accounts WHERE name = ?`, name); err != nil {
		return err
	}
	return tx.Commit()
}
