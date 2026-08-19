// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"database/sql"
	"time"
)

// Draft is a compose draft. The row is the source of truth locally and a
// write-through cache of the account's IMAP Drafts folder: saving writes here
// first and publishes in the background (see mail.Manager.PushDraft).
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

	// MessageID is the draft's identity across revisions: every save appends a
	// new copy to Drafts and expunges the old one, so the UID churns and this
	// does not. Empty until the first successful push generates one.
	MessageID string
	// FolderID/UID locate the revision currently on the server. 0 means never
	// published — or published to a server that reported no APPENDUID, in which
	// case the next sync resolves it by MessageID.
	FolderID  int64
	UID       uint32
	IMAPDirty bool // this content is not on the server yet; the worker retries it
}

// UpsertDraft inserts a new draft (ID == 0, the assigned id is written back)
// or updates one in place, bumping updated_at either way. Every write marks the
// row imap_dirty: the content just changed, so the published revision is stale
// until PushDraft replaces it. The server-side columns are deliberately not
// written here — they belong to the push, which is the only thing that knows
// what actually landed.
func (s *Store) UpsertDraft(d *Draft) error {
	now := time.Now().UTC().Format(time.RFC3339)
	d.UpdatedAt, _ = time.Parse(time.RFC3339, now)
	d.IMAPDirty = true
	if d.ID == 0 {
		res, err := s.DB.Exec(`
			INSERT INTO drafts (account, to_addresses, cc_addresses, bcc_addresses, subject, body, in_reply_to, kind, mode, updated_at, imap_dirty)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
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
			subject = ?, body = ?, in_reply_to = ?, kind = ?, mode = ?, updated_at = ?, imap_dirty = 1 WHERE id = ?`,
		d.Account, d.To, d.Cc, d.Bcc, d.Subject, d.Body, d.InReplyTo, d.Kind, d.Mode, now, d.ID)
	return err
}

const draftCols = `id, account, to_addresses, cc_addresses, bcc_addresses, subject, body, in_reply_to, kind, mode, updated_at, message_id, folder_id, uid, imap_dirty`

func scanDraft(sc interface{ Scan(...any) error }) (*Draft, error) {
	d := &Draft{}
	var updated string
	if err := sc.Scan(&d.ID, &d.Account, &d.To, &d.Cc, &d.Bcc, &d.Subject, &d.Body, &d.InReplyTo, &d.Kind, &d.Mode, &updated,
		&d.MessageID, &d.FolderID, &d.UID, &d.IMAPDirty); err != nil {
		return nil, err
	}
	d.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return d, nil
}

// DirtyDrafts returns an account's drafts whose IMAP push is still owed, oldest
// first, for the worker to retry — the drafts twin of DirtySeen.
func (s *Store) DirtyDrafts(account string, limit int) ([]Draft, error) {
	rows, err := s.DB.Query(`SELECT `+draftCols+` FROM drafts
		WHERE account = ? AND imap_dirty = 1 ORDER BY id LIMIT ?`, account, limit)
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

// ClearDraftDirty records where a just-published revision landed and, if the
// draft has not been edited since (updatedAt still matches what was pushed),
// marks it clean. A save that raced the push leaves it dirty so the next cycle
// republishes — but the location is still stored either way, because that copy
// is on the server and the next revision has to expunge it.
func (s *Store) ClearDraftDirty(id int64, messageID string, folderID int64, uid uint32, updatedAt time.Time) error {
	_, err := s.DB.Exec(`UPDATE drafts SET message_id = ?, folder_id = ?, uid = ?,
		imap_dirty = CASE WHEN updated_at = ? THEN 0 ELSE 1 END WHERE id = ?`,
		messageID, folderID, uid, updatedAt.UTC().Format(time.RFC3339), id)
	return err
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
