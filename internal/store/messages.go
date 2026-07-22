package store

import (
	"database/sql"
	"time"
)

type Message struct {
	ID            int64
	Account       string
	FolderID      int64
	UID           uint32
	MessageID     string
	InReplyTo     string
	Refs          string
	FromName      string
	FromAddress   string
	ToAddresses   string
	CcAddresses   string
	Subject       string
	Date          time.Time
	Size          int64
	IsRead        bool
	IsStarred     bool
	HasAttachment bool
	Snippet       string
	GmThrID       string // Gmail X-GM-THRID, "" when unavailable
	Labels        string // space-joined Gmail labels, "" for non-Gmail
}

// UpsertMessage inserts a message or, on UID conflict, updates the mutable
// flag/metadata fields (envelope is immutable so it is only set on insert).
func (s *Store) UpsertMessage(m *Message) error {
	_, err := s.DB.Exec(`
		INSERT INTO messages
			(account, folder_id, uid, message_id, in_reply_to, refs, from_name, from_address,
			 to_addresses, cc_addresses, subject, date, size, is_read, is_starred, has_attachment, snippet, gm_thrid, labels)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(folder_id, uid) DO UPDATE SET
			is_read = excluded.is_read, is_starred = excluded.is_starred,
			has_attachment = excluded.has_attachment, snippet = excluded.snippet, labels = excluded.labels`,
		m.Account, m.FolderID, m.UID, m.MessageID, m.InReplyTo, m.Refs, m.FromName, m.FromAddress,
		m.ToAddresses, m.CcAddresses, m.Subject, m.Date.UTC().Format(time.RFC3339), m.Size,
		b2i(m.IsRead), b2i(m.IsStarred), b2i(m.HasAttachment), m.Snippet, m.GmThrID, m.Labels)
	return err
}

func scanMessage(sc interface{ Scan(...any) error }) (*Message, error) {
	m := &Message{}
	var date string
	err := sc.Scan(&m.ID, &m.Account, &m.FolderID, &m.UID, &m.MessageID, &m.InReplyTo, &m.Refs,
		&m.FromName, &m.FromAddress, &m.ToAddresses, &m.CcAddresses, &m.Subject, &date, &m.Size,
		&m.IsRead, &m.IsStarred, &m.HasAttachment, &m.Snippet, &m.GmThrID, &m.Labels)
	if err != nil {
		return nil, err
	}
	m.Date, _ = time.Parse(time.RFC3339, date)
	return m, nil
}

const messageCols = `id, account, folder_id, uid, message_id, in_reply_to, refs, from_name,
	from_address, to_addresses, cc_addresses, subject, date, size, is_read, is_starred, has_attachment, snippet, gm_thrid, labels`

func (s *Store) MessageByID(id int64) (*Message, error) {
	m, err := scanMessage(s.DB.QueryRow(`SELECT `+messageCols+` FROM messages WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// ListMessages returns a folder's messages newest first, capped at limit.
func (s *Store) ListMessages(folderID int64, limit int) ([]Message, error) {
	rows, err := s.DB.Query(`SELECT `+messageCols+` FROM messages
		WHERE folder_id = ? ORDER BY date DESC LIMIT ?`, folderID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// ListUnifiedInbox returns messages from every account's inbox folder,
// newest first, capped at limit — the unified "All inboxes" view.
func (s *Store) ListUnifiedInbox(limit int) ([]Message, error) {
	rows, err := s.DB.Query(`SELECT `+messageCols+` FROM messages
		WHERE folder_id IN (SELECT id FROM folders WHERE special_use = 'inbox')
		ORDER BY date DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// SetLabels stores the space-joined Gmail label set for a message.
func (s *Store) SetLabels(id int64, labels string) error {
	_, err := s.DB.Exec(`UPDATE messages SET labels = ? WHERE id = ?`, labels, id)
	return err
}

// FolderUIDs returns the set of UIDs currently stored for a folder, for
// reconciling against the server (detecting expunged messages).
func (s *Store) FolderUIDs(folderID int64) (map[uint32]bool, error) {
	rows, err := s.DB.Query(`SELECT uid FROM messages WHERE folder_id = ?`, folderID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[uint32]bool{}
	for rows.Next() {
		var u uint32
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out[u] = true
	}
	return out, rows.Err()
}

// MaxUID returns the highest stored UID in a folder, 0 if empty.
func (s *Store) MaxUID(folderID int64) (uint32, error) {
	var u sql.NullInt64
	if err := s.DB.QueryRow(`SELECT MAX(uid) FROM messages WHERE folder_id = ?`, folderID).Scan(&u); err != nil {
		return 0, err
	}
	return uint32(u.Int64), nil // #nosec G115 -- UID fits uint32 by protocol
}

func (s *Store) SetRead(id int64, read bool) error {
	_, err := s.DB.Exec(`UPDATE messages SET is_read = ? WHERE id = ?`, b2i(read), id)
	return err
}

func (s *Store) SetStarred(id int64, starred bool) error {
	_, err := s.DB.Exec(`UPDATE messages SET is_starred = ? WHERE id = ?`, b2i(starred), id)
	return err
}

// DeleteMessageByUID removes a single message row (used when the server
// reports it moved/expunged).
func (s *Store) DeleteMessageByUID(folderID int64, uid uint32) error {
	_, err := s.DB.Exec(`DELETE FROM messages WHERE folder_id = ? AND uid = ?`, folderID, uid)
	return err
}

func (s *Store) DeleteMessage(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM messages WHERE id = ?`, id)
	return err
}

// SetMessageFolder relocates a message's local row to another folder, used for
// the optimistic move/undo-move flow (the real IMAP move is deferred; see
// Server.schedulePendingMove).
func (s *Store) SetMessageFolder(id, folderID int64) error {
	_, err := s.DB.Exec(`UPDATE messages SET folder_id = ? WHERE id = ?`, folderID, id)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
