// Package store is the SQLite repository layer.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	DB *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// NOTE: single connection avoids SQLITE_BUSY dances; revisit if it becomes a bottleneck.
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) migrate() error {
	if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var done int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&done); err != nil {
			return err
		}
		if done > 0 {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// --- users ---

type User struct {
	ID           int64
	Username     string
	PasswordHash string
}

func (s *Store) UserCount() (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// MessageCount is the total synced message count, shown next to the DB size
// in Settings → Syncing.
func (s *Store) MessageCount() (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(username, passwordHash string) error {
	_, err := s.DB.Exec(`INSERT INTO users (username, password_hash) VALUES (?, ?)`, username, passwordHash)
	return err
}

func (s *Store) UserByName(username string) (*User, error) {
	u := &User{}
	err := s.DB.QueryRow(`SELECT id, username, password_hash FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// --- sessions ---

func (s *Store) CreateSession(token string, userID int64, expires time.Time) error {
	_, err := s.DB.Exec(`INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)`,
		token, userID, expires.UTC().Format(time.RFC3339))
	return err
}

// SessionUserID returns the user id for a live session, 0 if absent/expired.
func (s *Store) SessionUserID(token string) (int64, error) {
	var id int64
	var expires string
	err := s.DB.QueryRow(`SELECT user_id, expires_at FROM sessions WHERE token = ?`, token).Scan(&id, &expires)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	t, err := time.Parse(time.RFC3339, expires)
	if err != nil || time.Now().After(t) {
		return 0, nil
	}
	return id, nil
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}
