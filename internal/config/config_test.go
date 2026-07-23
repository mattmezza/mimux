package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// Zero env vars (except a temp DB so the generated secret lands in TempDir).
	dir := t.TempDir()
	t.Setenv("SM_DB", filepath.Join(dir, "sm.db"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Host != "0.0.0.0" || cfg.Server.Port != 8083 {
		t.Errorf("server defaults = %+v", cfg.Server)
	}
	if cfg.Server.BaseURL != "http://localhost:8083" {
		t.Errorf("base_url default = %q", cfg.Server.BaseURL)
	}
	if cfg.Server.Secret == "" {
		t.Error("secret not generated")
	}
	// Secret persisted next to the DB and reused on the next Load.
	if _, err := os.Stat(filepath.Join(dir, "secret")); err != nil {
		t.Errorf("secret file not persisted: %v", err)
	}
	cfg2, _ := Load()
	if cfg2.Server.Secret != cfg.Server.Secret {
		t.Error("secret not stable across loads")
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("SM_DB", filepath.Join(t.TempDir(), "x.db"))
	t.Setenv("SM_HOST", "127.0.0.1")
	t.Setenv("SM_PORT", "9099")
	t.Setenv("SM_BASE_URL", "https://mail.example.com")
	t.Setenv("SM_SECRET", "explicit-secret")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Host != "127.0.0.1" || cfg.Server.Port != 9099 ||
		cfg.Server.BaseURL != "https://mail.example.com" || cfg.Server.Secret != "explicit-secret" {
		t.Errorf("env overrides not applied: %+v", cfg.Server)
	}
}

func TestLoadRejectsBadPort(t *testing.T) {
	t.Setenv("SM_DB", filepath.Join(t.TempDir(), "x.db"))
	t.Setenv("SM_PORT", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid SM_PORT")
	}
}

func TestNormalizeAccount(t *testing.T) {
	// Preset fills hosts and default auth.
	a := Account{Name: "g", Provider: "gmail", Email: "me@gmail.com"}
	if err := NormalizeAccount(&a); err != nil {
		t.Fatal(err)
	}
	if a.IMAPHost != "imap.gmail.com" || a.SMTPHost != "smtp.gmail.com" || a.Auth != "password" {
		t.Errorf("preset not applied: %+v", a)
	}
	// Explicit hosts preserved.
	b := Account{Name: "c", Email: "a@b.c", IMAPHost: "imap.b.c", SMTPHost: "smtp.b.c"}
	if err := NormalizeAccount(&b); err != nil {
		t.Fatal(err)
	}
	// No preset and no hosts is an error.
	c := Account{Name: "broken", Email: "a@b.c"}
	if err := NormalizeAccount(&c); err == nil {
		t.Fatal("expected error for account without hosts")
	}
}
