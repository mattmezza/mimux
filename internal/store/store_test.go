// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrateIdempotent(t *testing.T) {
	s := open(t)
	if err := s.migrate(); err != nil { // second run must be a no-op
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("no migrations recorded")
	}
}

func TestUsersAndSessions(t *testing.T) {
	s := open(t)
	if n, _ := s.UserCount(); n != 0 {
		t.Fatalf("fresh db has %d users", n)
	}
	if err := s.CreateUser("matteo", "hash"); err != nil {
		t.Fatal(err)
	}
	u, err := s.UserByName("matteo")
	if err != nil || u == nil || u.PasswordHash != "hash" {
		t.Fatalf("UserByName = %+v, %v", u, err)
	}
	if missing, _ := s.UserByName("nobody"); missing != nil {
		t.Error("nonexistent user found")
	}

	// live session
	if err := s.CreateSession("tok", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.SessionUserID("tok"); id != u.ID {
		t.Errorf("SessionUserID = %d, want %d", id, u.ID)
	}
	// expired session
	_ = s.CreateSession("old", u.ID, time.Now().Add(-time.Hour))
	if id, _ := s.SessionUserID("old"); id != 0 {
		t.Error("expired session accepted")
	}
	// deleted session
	_ = s.DeleteSession("tok")
	if id, _ := s.SessionUserID("tok"); id != 0 {
		t.Error("deleted session accepted")
	}
}
