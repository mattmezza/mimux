package store

import "database/sql"

type Folder struct {
	ID            int64
	Account       string
	Name          string
	SpecialUse    string
	UIDValidity   uint32
	HighestModSeq uint64
	UnreadCount   int
	Sort          int
}

// UpsertFolder inserts or updates a folder by (account, name), preserving the
// id and returning it. UIDVALIDITY/HIGHESTMODSEQ are managed separately by sync.
func (s *Store) UpsertFolder(account, name, specialUse string, sort int) (int64, error) {
	_, err := s.DB.Exec(`
		INSERT INTO folders (account, name, special_use, sort) VALUES (?, ?, ?, ?)
		ON CONFLICT(account, name) DO UPDATE SET special_use = excluded.special_use, sort = excluded.sort`,
		account, name, specialUse, sort)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.DB.QueryRow(`SELECT id FROM folders WHERE account = ? AND name = ?`, account, name).Scan(&id)
	return id, err
}

func (s *Store) FolderByID(id int64) (*Folder, error) {
	f := &Folder{}
	err := s.DB.QueryRow(`SELECT id, account, name, special_use, uidvalidity, highestmodseq, unread_count, sort
		FROM folders WHERE id = ?`, id).
		Scan(&f.ID, &f.Account, &f.Name, &f.SpecialUse, &f.UIDValidity, &f.HighestModSeq, &f.UnreadCount, &f.Sort)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}

// FolderByName returns the account's folder with the exact IMAP mailbox
// name, nil if none. Used to resolve filter "move" actions that name a
// folder rather than a special-use role.
func (s *Store) FolderByName(account, name string) (*Folder, error) {
	f := &Folder{}
	err := s.DB.QueryRow(`SELECT id, account, name, special_use, uidvalidity, highestmodseq, unread_count, sort
		FROM folders WHERE account = ? AND name = ?`, account, name).
		Scan(&f.ID, &f.Account, &f.Name, &f.SpecialUse, &f.UIDValidity, &f.HighestModSeq, &f.UnreadCount, &f.Sort)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}

// FolderBySpecial returns the account's folder with the given special-use, nil if none.
func (s *Store) FolderBySpecial(account, special string) (*Folder, error) {
	f := &Folder{}
	err := s.DB.QueryRow(`SELECT id, account, name, special_use, uidvalidity, highestmodseq, unread_count, sort
		FROM folders WHERE account = ? AND special_use = ?`, account, special).
		Scan(&f.ID, &f.Account, &f.Name, &f.SpecialUse, &f.UIDValidity, &f.HighestModSeq, &f.UnreadCount, &f.Sort)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}

// ListFolders returns an account's folders, special-use ones first (by a fixed
// order), then the rest alphabetically.
func (s *Store) ListFolders(account string) ([]Folder, error) {
	rows, err := s.DB.Query(`SELECT id, account, name, special_use, uidvalidity, highestmodseq, unread_count, sort
		FROM folders WHERE account = ? ORDER BY sort, name COLLATE NOCASE`, account)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.Account, &f.Name, &f.SpecialUse, &f.UIDValidity, &f.HighestModSeq, &f.UnreadCount, &f.Sort); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FirstInbox returns the first account's inbox folder, or the first folder of
// any kind — the default landing folder. Returns nil if no folders exist yet.
func (s *Store) FirstInbox() (*Folder, error) {
	f := &Folder{}
	err := s.DB.QueryRow(`SELECT id, account, name, special_use, uidvalidity, highestmodseq, unread_count, sort
		FROM folders ORDER BY (special_use = 'inbox') DESC, sort, id LIMIT 1`).
		Scan(&f.ID, &f.Account, &f.Name, &f.SpecialUse, &f.UIDValidity, &f.HighestModSeq, &f.UnreadCount, &f.Sort)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}

func (s *Store) SetFolderUIDValidity(id int64, uidvalidity uint32) error {
	_, err := s.DB.Exec(`UPDATE folders SET uidvalidity = ? WHERE id = ?`, uidvalidity, id)
	return err
}

func (s *Store) SetFolderModSeq(id int64, modseq uint64) error {
	_, err := s.DB.Exec(`UPDATE folders SET highestmodseq = ? WHERE id = ?`, modseq, id)
	return err
}

// RecountUnread recomputes and stores unread_count from the messages table.
func (s *Store) RecountUnread(id int64) error {
	_, err := s.DB.Exec(`UPDATE folders SET unread_count =
		(SELECT COUNT(*) FROM messages WHERE folder_id = ? AND is_read = 0) WHERE id = ?`, id, id)
	return err
}

// ClearFolderMessages drops all messages in a folder (UIDVALIDITY change reset).
func (s *Store) ClearFolderMessages(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM messages WHERE folder_id = ?`, id)
	return err
}
