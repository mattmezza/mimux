package store

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/mattmezza/sm/internal/config"
)

// accountCols is the column list shared by the account queries.
const accountCols = `name, label, sender_name, provider, email, auth, password,
	oauth2_client_id, oauth2_client_secret, imap_host, imap_port,
	smtp_host, smtp_port, aliases, position,
	sync_interval_min, max_per_sync, sync_months, body_cache`

func scanAccount(sc interface{ Scan(...any) error }) (config.Account, int, error) {
	var a config.Account
	var aliasesJSON string
	var pos int
	var syncInterval, maxPerSync, syncMonths, bodyCache sql.NullInt64
	err := sc.Scan(&a.Name, &a.Label, &a.SenderName, &a.Provider, &a.Email, &a.Auth, &a.Password,
		&a.OAuth2ClientID, &a.OAuth2ClientSecret, &a.IMAPHost, &a.IMAPPort,
		&a.SMTPHost, &a.SMTPPort, &aliasesJSON, &pos,
		&syncInterval, &maxPerSync, &syncMonths, &bodyCache)
	if err != nil {
		return a, 0, err
	}
	if aliasesJSON != "" {
		_ = json.Unmarshal([]byte(aliasesJSON), &a.Aliases)
	}
	a.SyncIntervalMin = nullIntPtr(syncInterval)
	a.MaxPerSync = nullIntPtr(maxPerSync)
	a.SyncMonths = nullIntPtr(syncMonths)
	a.BodyCache = nullIntPtr(bodyCache)
	return a, pos, nil
}

// nullIntPtr converts a nullable column into the override-pointer shape used
// by config.Account (nil = "inherit the global value").
func nullIntPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// intOrNull is the inverse of nullIntPtr: a nil override binds as SQL NULL.
func intOrNull(p *int) any {
	if p == nil {
		return nil
	}
	return *p
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
	// sync_interval_min/max_per_sync/sync_months/body_cache are deliberately
	// absent from the ON CONFLICT SET list (like position): they're edited
	// separately from Settings → Syncing, so a plain account-form save must not
	// clobber them.
	_, err = s.DB.Exec(`
		INSERT INTO accounts (`+accountCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?, COALESCE((SELECT position FROM accounts WHERE name = ?), (SELECT COALESCE(MAX(position),0)+1 FROM accounts)), ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			label = excluded.label,
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
		a.Name, a.Label, a.SenderName, a.Provider, a.Email, a.Auth, a.Password,
		a.OAuth2ClientID, a.OAuth2ClientSecret, a.IMAPHost, a.IMAPPort,
		a.SMTPHost, a.SMTPPort, string(b), a.Name,
		intOrNull(a.SyncIntervalMin), intOrNull(a.MaxPerSync), intOrNull(a.SyncMonths),
		intOrNull(a.BodyCache))
	return err
}

// SetAccountSyncOverrides updates only the per-account sync-override columns
// (nil clears an override back to "inherit global"). Kept separate from
// UpsertAccount, which never touches these, so Settings → Syncing can't
// clobber the rest of the account and vice versa.
func (s *Store) SetAccountSyncOverrides(name string, intervalMin, maxPerSync, syncMonths, bodyCache *int) error {
	_, err := s.DB.Exec(`UPDATE accounts SET sync_interval_min = ?, max_per_sync = ?, sync_months = ?, body_cache = ? WHERE name = ?`,
		intOrNull(intervalMin), intOrNull(maxPerSync), intOrNull(syncMonths), intOrNull(bodyCache), name)
	return err
}

// EffectiveSyncSettings resolves the sync-interval/max-per-sync/sync-months/
// body-cache knobs for one account: its own override where set, else the global
// Settings → Syncing value. A pure function of Prefs + Account so it's cheap
// to unit test without a live DB.
func EffectiveSyncSettings(p Prefs, a config.Account) (intervalMin, maxPerSync, syncMonths, bodyCache int) {
	intervalMin, maxPerSync, syncMonths, bodyCache = p.SyncIntervalMin, p.MaxPerSync, p.SyncMonths, p.BodyCache
	if a.SyncIntervalMin != nil {
		intervalMin = *a.SyncIntervalMin
	}
	if a.MaxPerSync != nil {
		maxPerSync = *a.MaxPerSync
	}
	if a.SyncMonths != nil {
		syncMonths = *a.SyncMonths
	}
	if a.BodyCache != nil {
		bodyCache = *a.BodyCache
	}
	return
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

// AccountStat is the stored-mail footprint of one account, for the accounts
// dialog in the status bar.
type AccountStat struct {
	Messages int64
	Unread   int64
	Folders  int64
	Bytes    int64 // summed RFC822 size as IMAP reports it — mail size, not disk (see DBSize)
}

// DBSize is the size of the SQLite file itself — what "how much disk is this
// costing me" actually means. AccountStat.Bytes is a different number: the
// server-reported RFC822 size of the mail, most of which (attachments, and any
// body that isn't cached) never lands here.
func (s *Store) DBSize() int64 {
	var pages, size int64
	_ = s.DB.QueryRow(`PRAGMA page_count`).Scan(&pages)
	_ = s.DB.QueryRow(`PRAGMA page_size`).Scan(&size)
	return pages * size
}

// AccountStats returns per-account counters keyed by account name. Two grouped
// queries, not one per account: messages carry count+size, folders carry the
// folder count and the unread_count sync already maintains.
// ponytail: no outbox/body-cache columns here — one more table, one more query,
// and neither number is what the dialog is for.
func (s *Store) AccountStats() (map[string]AccountStat, error) {
	out := map[string]AccountStat{}
	for _, q := range []struct {
		sql  string
		scan func(*AccountStat, int64, int64)
	}{
		{`SELECT account, COUNT(*), COALESCE(SUM(size), 0) FROM messages GROUP BY account`,
			func(st *AccountStat, a, b int64) { st.Messages, st.Bytes = a, b }},
		{`SELECT account, COUNT(*), COALESCE(SUM(unread_count), 0) FROM folders GROUP BY account`,
			func(st *AccountStat, a, b int64) { st.Folders, st.Unread = a, b }},
	} {
		rows, err := s.DB.Query(q.sql)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			var a, b int64
			if err := rows.Scan(&name, &a, &b); err != nil {
				_ = rows.Close()
				return nil, err
			}
			st := out[name]
			q.scan(&st, a, b)
			out[name] = st
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}
