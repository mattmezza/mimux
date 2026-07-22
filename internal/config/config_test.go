package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	cfg, err := Load(write(t, `
[server]
secret = "s3cret"
port = 9090

[[accounts]]
name = "Personal"
provider = "gmail"
email = "me@gmail.com"
auth = "oauth2"

[[accounts]]
name = "Privacy"
email = "me@mydomain.com"
password = "pw"
imap_host = "imap.example.com"
imap_port = 993
smtp_host = "smtp.example.com"
smtp_port = 465

[sync]
poll_interval = "2m"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 9090 || cfg.Server.Host != "0.0.0.0" {
		t.Errorf("server = %+v", cfg.Server)
	}
	// preset fill-in
	a := cfg.Accounts[0]
	if a.IMAPHost != "imap.gmail.com" || a.IMAPPort != 993 || a.SMTPHost != "smtp.gmail.com" {
		t.Errorf("gmail preset not applied: %+v", a)
	}
	// explicit hosts kept, default auth
	b := cfg.Accounts[1]
	if b.IMAPHost != "imap.example.com" || b.Auth != "password" {
		t.Errorf("explicit account mangled: %+v", b)
	}
	if cfg.Sync.Interval() != 2*time.Minute {
		t.Errorf("poll_interval = %v", cfg.Sync.Interval())
	}
	if cfg.AI.Model == "" || cfg.Translate.TargetLanguage != "en" {
		t.Errorf("defaults missing: ai=%+v translate=%+v", cfg.AI, cfg.Translate)
	}
}

func TestLoadRejectsMissingSecret(t *testing.T) {
	if _, err := Load(write(t, "[server]\nport = 1\n")); err == nil {
		t.Fatal("expected error for missing secret")
	}
}

func TestLoadRejectsUnknownProviderWithoutHosts(t *testing.T) {
	if _, err := Load(write(t, `
[server]
secret = "x"
[[accounts]]
name = "Broken"
email = "a@b.c"
`)); err == nil {
		t.Fatal("expected error for account without hosts")
	}
}
