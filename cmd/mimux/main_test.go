// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The rename is the one startup step that can look like total data loss if it
// silently no-ops, so it gets a check.
// TODO(v0.21): delete with migrateLegacyDB.
func TestMigrateLegacyDB(t *testing.T) {
	dir := t.TempDir()
	oldPath, newPath := filepath.Join(dir, "sm.db"), filepath.Join(dir, "mimux.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(oldPath+suffix, []byte("mail"+suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	migrateLegacyDB(newPath)

	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(newPath + suffix) // #nosec G304 -- t.TempDir() path
		if err != nil {
			t.Fatalf("%s not migrated: %v", newPath+suffix, err)
		}
		if string(b) != "mail"+suffix {
			t.Errorf("%s = %q, want the old file's contents", newPath+suffix, b)
		}
		if _, err := os.Stat(oldPath + suffix); !os.IsNotExist(err) {
			t.Errorf("%s still there after migration", oldPath+suffix)
		}
	}
}

// A populated new DB must never be clobbered by a leftover old one.
func TestMigrateLegacyDBLeavesExistingAlone(t *testing.T) {
	dir := t.TempDir()
	oldPath, newPath := filepath.Join(dir, "sm.db"), filepath.Join(dir, "mimux.db")
	if err := os.WriteFile(oldPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}

	migrateLegacyDB(newPath)

	if b, _ := os.ReadFile(newPath); string(b) != "current" { // #nosec G304 -- t.TempDir() path
		t.Errorf("existing db overwritten: %q", b)
	}
}

// A custom filename isn't ours to guess an "old" name for.
func TestMigrateLegacyDBIgnoresCustomPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sm.db"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	migrateLegacyDB(filepath.Join(dir, "custom.db"))
	if _, err := os.Stat(filepath.Join(dir, "sm.db")); err != nil {
		t.Errorf("custom path triggered a migration: %v", err)
	}
}
