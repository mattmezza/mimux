package store

import (
	"database/sql"
	"time"
)

// GetMessageBody returns the cached gob-encoded parsed body for a message, and
// whether one was present.
func (s *Store) GetMessageBody(id int64) ([]byte, bool, error) {
	var blob []byte
	err := s.DB.QueryRow(`SELECT body FROM message_bodies WHERE message_id = ?`, id).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return blob, true, nil
}

// SaveMessageBody stores (or overwrites) the cached body blob for a message.
func (s *Store) SaveMessageBody(id int64, blob []byte) error {
	_, err := s.DB.Exec(`INSERT INTO message_bodies (message_id, body, fetched_at) VALUES (?, ?, ?)
		ON CONFLICT(message_id) DO UPDATE SET body = excluded.body, fetched_at = excluded.fetched_at`,
		id, blob, time.Now().Unix())
	return err
}

// MessagesWithoutBody returns a folder's messages that have no cached body yet,
// newest first — the background warmer's work queue. window bounds the search
// to the folder's newest N messages (the body-cache size), so the warmer can
// never work back through the whole synced history; limit is one batch of that
// window. The window is the same set PruneMessageBodies keeps, so the two can't
// fight over a message.
func (s *Store) MessagesWithoutBody(folderID int64, window, limit int) ([]Message, error) {
	rows, err := s.DB.Query(`SELECT `+messageCols+` FROM messages
		WHERE folder_id = ? AND id NOT IN (SELECT message_id FROM message_bodies)
		AND id IN (SELECT id FROM messages WHERE folder_id = ? ORDER BY date DESC LIMIT ?)
		ORDER BY date DESC LIMIT ?`, folderID, folderID, window, limit)
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

// PruneMessageBodies drops cached bodies for everything outside a folder's
// newest keep messages, returning how many rows went. keep = 0 clears the
// folder's cache entirely ("off"). Dropping a body is cheap: opening the
// message re-fetches and re-caches it.
func (s *Store) PruneMessageBodies(folderID int64, keep int) (int64, error) {
	res, err := s.DB.Exec(`DELETE FROM message_bodies WHERE message_id IN (
		SELECT id FROM messages WHERE folder_id = ?
		AND id NOT IN (SELECT id FROM messages WHERE folder_id = ? ORDER BY date DESC LIMIT ?))`,
		folderID, folderID, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteMessageBody drops a message's cached body.
func (s *Store) DeleteMessageBody(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM message_bodies WHERE message_id = ?`, id)
	return err
}
