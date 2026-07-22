package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/mattmezza/sm/internal/search"
)

// SearchLocal runs an instant, index-backed search over the cached messages.
func (s *Store) SearchLocal(q *search.SearchQuery, scope search.Scope, account string, folderID int64, limit int) ([]Message, error) {
	where, args := search.BuildLocalSQL(q, scope, account, folderID)
	args = append(args, limit)
	// #nosec G202 -- `where` is assembled from fixed fragments by BuildLocalSQL; every user value is a bound ? parameter in args.
	rows, err := s.DB.Query(`SELECT `+messageCols+` FROM messages WHERE `+where+`
		ORDER BY date DESC LIMIT ?`, args...)
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

// MessagesByIDs loads messages by id, preserving the given order.
func (s *Store) MessagesByIDs(ids []int64) ([]Message, error) {
	byID := make(map[int64]Message, len(ids))
	for _, id := range ids {
		m, err := s.MessageByID(id)
		if err != nil {
			return nil, err
		}
		if m != nil {
			byID[id] = *m
		}
	}
	out := make([]Message, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// MessageByFolderUID looks up a cached message by (folder, uid), nil if absent.
func (s *Store) MessageByFolderUID(folderID int64, uid uint32) (*Message, error) {
	m, err := scanMessage(s.DB.QueryRow(`SELECT `+messageCols+` FROM messages WHERE folder_id = ? AND uid = ?`, folderID, uid))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// --- server-search cache ---

// QueryHash keys the server-search cache by (query, scope, account, folder).
func QueryHash(raw string, scope search.Scope, account string, folderID int64) string {
	h := sha256.Sum256([]byte(string(scope) + "\x00" + account + "\x00" + raw))
	_ = folderID // ponytail: folder scope already narrows the query text via in:; hash on the text keeps it simple.
	return hex.EncodeToString(h[:])
}

// SearchCacheGet returns the cached ids for a query hash if newer than ttl.
func (s *Store) SearchCacheGet(hash string, ttl time.Duration) ([]int64, bool) {
	var idsJSON, created string
	err := s.DB.QueryRow(`SELECT message_ids, created_at FROM search_cache WHERE query_hash = ?`, hash).Scan(&idsJSON, &created)
	if err != nil {
		return nil, false
	}
	t, err := time.Parse("2006-01-02 15:04:05", created)
	if err != nil || time.Since(t) > ttl {
		return nil, false
	}
	var ids []int64
	if json.Unmarshal([]byte(idsJSON), &ids) != nil {
		return nil, false
	}
	return ids, true
}

// SearchCachePut upserts the cached ids for a query hash.
func (s *Store) SearchCachePut(hash string, ids []int64) error {
	b, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO search_cache (query_hash, message_ids, created_at)
		VALUES (?, ?, datetime('now'))
		ON CONFLICT(query_hash) DO UPDATE SET message_ids = excluded.message_ids, created_at = excluded.created_at`,
		hash, string(b))
	return err
}

// --- search history ---

// AddHistory records a query, de-duplicating consecutive repeats and capping
// the table at 50 rows.
func (s *Store) AddHistory(query string) error {
	if query == "" {
		return nil
	}
	if _, err := s.DB.Exec(`DELETE FROM search_history WHERE query = ?`, query); err != nil {
		return err
	}
	if _, err := s.DB.Exec(`INSERT INTO search_history (query) VALUES (?)`, query); err != nil {
		return err
	}
	_, err := s.DB.Exec(`DELETE FROM search_history WHERE id NOT IN
		(SELECT id FROM search_history ORDER BY id DESC LIMIT 50)`)
	return err
}

// ListHistory returns the most recent queries, newest first.
func (s *Store) ListHistory(limit int) ([]string, error) {
	rows, err := s.DB.Query(`SELECT query FROM search_history ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *Store) ClearHistory() error {
	_, err := s.DB.Exec(`DELETE FROM search_history`)
	return err
}

// --- saved searches ---

type SavedSearch struct {
	ID    int64
	Name  string
	Query string
}

func (s *Store) SaveSearch(name, query string) error {
	_, err := s.DB.Exec(`INSERT INTO saved_searches (name, query) VALUES (?, ?)`, name, query)
	return err
}

func (s *Store) ListSavedSearches() ([]SavedSearch, error) {
	rows, err := s.DB.Query(`SELECT id, name, query FROM saved_searches ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SavedSearch
	for rows.Next() {
		var ss SavedSearch
		if err := rows.Scan(&ss.ID, &ss.Name, &ss.Query); err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSavedSearch(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM saved_searches WHERE id = ?`, id)
	return err
}
