// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// Draft is a compose draft. The row is the source of truth locally and a
// write-through cache of the account's IMAP Drafts folder: saving writes here
// first and publishes in the background (see mail.Manager.PushDraft).
type Draft struct {
	ID                            int64
	Account                       string
	To                            string
	Cc                            string
	Bcc                           string
	Subject                       string
	Body                          string
	InReplyTo                     string
	Kind                          string // new|reply|reply_all|forward
	Mode                          string // plain|html|markdown — which editor authored Body
	ForwardSourceID               int64
	ForwardAttachments            []ForwardAttachment
	ForwardAttachmentsInitialized bool
	UpdatedAt                     time.Time

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

// ForwardAttachment identifies a selected attachment on the message being
// forwarded. It deliberately contains no bytes: those are fetched only when
// Send is pressed, then copied into the durable outbox when delivery is queued.
type ForwardAttachment struct {
	Part        []int  `json:"part"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        uint32 `json:"size"`
}

func (a ForwardAttachment) ByteSize() int64 { return int64(a.Size) }

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
	forwardJSON := ""
	if len(d.ForwardAttachments) > 0 {
		b, err := json.Marshal(d.ForwardAttachments)
		if err != nil {
			return err
		}
		forwardJSON = string(b)
	}
	if d.ID == 0 {
		res, err := s.DB.Exec(`
			INSERT INTO drafts (account, to_addresses, cc_addresses, bcc_addresses, subject, body, in_reply_to, kind, mode, forward_source_id, forward_attachments, forward_attachments_initialized, updated_at, imap_dirty)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
			d.Account, d.To, d.Cc, d.Bcc, d.Subject, d.Body, d.InReplyTo, d.Kind, d.Mode, d.ForwardSourceID, forwardJSON, d.ForwardAttachmentsInitialized, now)
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
			subject = ?, body = ?, in_reply_to = ?, kind = ?, mode = ?, forward_source_id = ?, forward_attachments = ?, forward_attachments_initialized = ?, updated_at = ?, imap_dirty = 1 WHERE id = ?`,
		d.Account, d.To, d.Cc, d.Bcc, d.Subject, d.Body, d.InReplyTo, d.Kind, d.Mode, d.ForwardSourceID, forwardJSON, d.ForwardAttachmentsInitialized, now, d.ID)
	return err
}

const draftCols = `id, account, to_addresses, cc_addresses, bcc_addresses, subject, body, in_reply_to, kind, mode, forward_source_id, forward_attachments, forward_attachments_initialized, updated_at, message_id, folder_id, uid, imap_dirty`

func scanDraft(sc interface{ Scan(...any) error }) (*Draft, error) {
	d := &Draft{}
	var updated, forwardJSON string
	if err := sc.Scan(&d.ID, &d.Account, &d.To, &d.Cc, &d.Bcc, &d.Subject, &d.Body, &d.InReplyTo, &d.Kind, &d.Mode, &d.ForwardSourceID, &forwardJSON, &d.ForwardAttachmentsInitialized, &updated,
		&d.MessageID, &d.FolderID, &d.UID, &d.IMAPDirty); err != nil {
		return nil, err
	}
	d.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	if forwardJSON != "" {
		_ = json.Unmarshal([]byte(forwardJSON), &d.ForwardAttachments)
	}
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
// SetDraftMessageID records the draft's identity on its own, before the first
// revision is appended. It is deliberately separate from ClearDraftDirty, which
// says "a revision landed HERE" and cannot honestly be called before the APPEND:
// a push interrupted in between would otherwise come back, find no Message-ID
// and mint a second one, orphaning the copy it already wrote.
func (s *Store) SetDraftMessageID(id int64, messageID string) error {
	_, err := s.DB.Exec(`UPDATE drafts SET message_id = ? WHERE id = ?`, messageID, id)
	return err
}

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

// DraftByServerCopy finds the local draft that owns a particular copy sitting
// in a mailbox: by Message-ID, which survives every revision, or — for a draft
// written elsewhere that carries none — by where the copy actually is. Nil
// means no local row owns it: a draft from another client, not yet adopted.
func (s *Store) DraftByServerCopy(account, messageID string, folderID int64, uid uint32) (*Draft, error) {
	d, err := scanDraft(s.DB.QueryRow(`SELECT `+draftCols+` FROM drafts
		WHERE account = ? AND ((message_id <> '' AND message_id = ?) OR (uid <> 0 AND folder_id = ? AND uid = ?))
		LIMIT 1`, account, messageID, folderID, uid))
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

// DraftAttachment is one file kept with a saved draft: the bytes live in
// SQLite (migration 0240) so they survive a reopen, ride along in the copy
// published to the IMAP Drafts folder, and are attached at send exactly like a
// file picked in the compose window.
type DraftAttachment struct {
	ID          int64
	Filename    string
	ContentType string
	Data        []byte
}

// Size is the file's size in bytes — a method so templates can format it
// without a conversion helper (len returns an int, humanBytes wants int64).
func (a DraftAttachment) Size() int64 { return int64(len(a.Data)) }

// AddDraftAttachment stores one file against a draft, writing back its id.
func (s *Store) AddDraftAttachment(draftID int64, a *DraftAttachment) error {
	res, err := s.DB.Exec(`INSERT INTO draft_attachments (draft_id, filename, content_type, content)
		VALUES (?, ?, ?, ?)`, draftID, a.Filename, a.ContentType, a.Data)
	if err != nil {
		return err
	}
	a.ID, err = res.LastInsertId()
	return err
}

// DraftAttachments returns a draft's files, oldest first, bytes included.
//
// ponytail: content is always read, even for the compose window's chip list
// which only wants names and sizes. The cap is 25MB per draft and this runs on
// a reopen/save/send, not in a list view — add a metadata-only query if a
// profile ever says otherwise.
func (s *Store) DraftAttachments(draftID int64) ([]DraftAttachment, error) {
	rows, err := s.DB.Query(`SELECT id, filename, content_type, content
		FROM draft_attachments WHERE draft_id = ? ORDER BY id`, draftID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DraftAttachment
	for rows.Next() {
		var a DraftAttachment
		if err := rows.Scan(&a.ID, &a.Filename, &a.ContentType, &a.Data); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DraftAttachmentsSize is the total bytes already stored against a draft, so
// the save path can enforce the send path's cap without reading the blobs.
func (s *Store) DraftAttachmentsSize(draftID int64) (int64, error) {
	var n int64
	err := s.DB.QueryRow(`SELECT COALESCE(SUM(LENGTH(content)), 0)
		FROM draft_attachments WHERE draft_id = ?`, draftID).Scan(&n)
	return n, err
}

// DeleteDraftAttachment removes one file from a draft. The draft id is part of
// the match so a stale id from one compose window cannot reach into another.
func (s *Store) DeleteDraftAttachment(draftID, id int64) error {
	_, err := s.DB.Exec(`DELETE FROM draft_attachments WHERE id = ? AND draft_id = ?`, id, draftID)
	return err
}
