package store

import (
	"database/sql"
	"time"
)

// Draft is a local (not IMAP-synced) compose draft, auto-saved while a
// message is being composed.
type Draft struct {
	ID        int64
	Account   string
	To        string
	Cc        string
	Bcc       string
	Subject   string
	Body      string
	InReplyTo string
	Kind      string // new|reply|reply_all|forward
	Mode      string // plain|html|markdown — which editor authored Body
	UpdatedAt time.Time
}

// UpsertDraft inserts a new draft (ID == 0, the assigned id is written back)
// or updates one in place, bumping updated_at either way.
func (s *Store) UpsertDraft(d *Draft) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if d.ID == 0 {
		res, err := s.DB.Exec(`
			INSERT INTO drafts (account, to_addresses, cc_addresses, bcc_addresses, subject, body, in_reply_to, kind, mode, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.Account, d.To, d.Cc, d.Bcc, d.Subject, d.Body, d.InReplyTo, d.Kind, d.Mode, now)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		d.ID = id
		return nil
	}
	_, err := s.DB.Exec(`
		UPDATE drafts SET account = ?, to_addresses = ?, cc_addresses = ?, bcc_addresses = ?,
			subject = ?, body = ?, in_reply_to = ?, kind = ?, mode = ?, updated_at = ? WHERE id = ?`,
		d.Account, d.To, d.Cc, d.Bcc, d.Subject, d.Body, d.InReplyTo, d.Kind, d.Mode, now, d.ID)
	return err
}

const draftCols = `id, account, to_addresses, cc_addresses, bcc_addresses, subject, body, in_reply_to, kind, mode, updated_at`

func scanDraft(sc interface{ Scan(...any) error }) (*Draft, error) {
	d := &Draft{}
	var updated string
	if err := sc.Scan(&d.ID, &d.Account, &d.To, &d.Cc, &d.Bcc, &d.Subject, &d.Body, &d.InReplyTo, &d.Kind, &d.Mode, &updated); err != nil {
		return nil, err
	}
	d.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return d, nil
}

func (s *Store) DraftByID(id int64) (*Draft, error) {
	d, err := scanDraft(s.DB.QueryRow(`SELECT `+draftCols+` FROM drafts WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

// ListDrafts returns every local draft, most recently updated first.
func (s *Store) ListDrafts() ([]Draft, error) {
	rows, err := s.DB.Query(`SELECT ` + draftCols + ` FROM drafts ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Draft
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

func (s *Store) DeleteDraft(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM drafts WHERE id = ?`, id)
	return err
}
