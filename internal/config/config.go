// Package config holds bootstrap config only — the settings that cannot live in
// the database (chicken-and-egg): db path, listen host/port, public base_url and
// the session secret. Everything else (accounts, sync cadence, translate/AI
// keys) lives in the DB and is edited from the Settings GUI.
//
// Bootstrap comes entirely from environment variables, each with a sane default,
// so a fresh install with ZERO env vars boots and works:
//
//	SM_DB       sqlite path            (default ./data/sm.db)
//	SM_HOST     bind address           (default 0.0.0.0)
//	SM_PORT     port                   (default 8083)
//	SM_BASE_URL public URL             (default http://localhost:<port>)
//	SM_SECRET   session/CSRF secret    (default: generated once, persisted next
//	                                     to the DB so sessions survive restarts)
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mattmezza/sm/internal/account"
)

type Config struct {
	Server Server
	DB     DB
	// Accounts is a runtime snapshot of the DB-backed accounts, refreshed by the
	// server whenever the account list changes. It is NOT parsed from any file —
	// it exists so the HTTP layer and compose selector can read the current
	// accounts through the existing cfg without per-request DB plumbing.
	Accounts []Account
}

type Server struct {
	Host    string
	Port    int
	BaseURL string
	Secret  string
}

type DB struct {
	Path string
}

// Account is one email account. It lives in the DB now (see internal/store);
// this type stays the in-memory representation the mail engine and templates use.
type Account struct {
	// Name is the account's identity/key (folders, tokens, colors, filters are
	// keyed by it), which is why it can't be renamed — see Label for the part
	// the user can change.
	Name string
	// Label is what the UI shows instead of Name. Blank inherits Name.
	Label string
	// SenderName is the display name on outgoing mail (the From header).
	SenderName         string
	Provider           string
	Email              string
	Aliases            []Alias // extra identities this account can send/receive as
	Auth               string  // "password" | "oauth2"
	Password           string
	OAuth2ClientID     string
	OAuth2ClientSecret string
	IMAPHost           string
	IMAPPort           int
	SMTPHost           string
	SMTPPort           int
	// Per-account overrides for the Settings → Syncing knobs; nil means
	// "inherit the global value" (see store.EffectiveSyncSettings).
	SyncIntervalMin *int
	MaxPerSync      *int
	SyncMonths      *int
	BodyCache       *int
}

// Alias is an extra send/receive identity on an account, with its own sender
// display name and address.
type Alias struct {
	Name  string // display name on outgoing mail sent as this alias
	Email string // the alias address
}

// DisplayLabel is the account's name as the UI should show it.
func (a Account) DisplayLabel() string {
	if a.Label != "" {
		return a.Label
	}
	return a.Name
}

// DisplayNameFor returns the From display name to use when sending as addr: the
// matching alias's name, else the account's own sender name (falling back to
// its label).
func (a Account) DisplayNameFor(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	for _, al := range a.Aliases {
		if strings.ToLower(strings.TrimSpace(al.Email)) == addr && al.Name != "" {
			return al.Name
		}
	}
	if a.SenderName != "" {
		return a.SenderName
	}
	return a.Name
}

// NormalizeAccount fills sender/identity fallbacks and provider-preset hosts,
// then validates that IMAP/SMTP hosts are known. Shared by the store loader and
// import so DB-driven accounts get the same treatment the old TOML loader gave.
func NormalizeAccount(a *Account) error {
	if a.Name == "" {
		a.Name = a.SenderName
	}
	if a.SenderName == "" {
		a.SenderName = a.Name
	}
	if p, ok := account.Presets[a.Provider]; ok {
		if a.IMAPHost == "" {
			a.IMAPHost, a.IMAPPort = p.IMAPHost, p.IMAPPort
		}
		if a.SMTPHost == "" {
			a.SMTPHost, a.SMTPPort = p.SMTPHost, p.SMTPPort
		}
	}
	if a.IMAPHost == "" || a.SMTPHost == "" {
		return fmt.Errorf("account %q: no provider preset and no imap/smtp host", a.Name)
	}
	if a.Auth == "" {
		a.Auth = "password"
	}
	return nil
}

// Load builds the bootstrap config from environment variables + defaults,
// generating and persisting a session secret when SM_SECRET is unset.
func Load() (*Config, error) {
	cfg := &Config{
		Server: Server{Host: "0.0.0.0", Port: 8083},
		DB:     DB{Path: "./data/sm.db"},
	}
	if v := os.Getenv("SM_DB"); v != "" {
		cfg.DB.Path = v
	}
	if v := os.Getenv("SM_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("SM_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config: SM_PORT %q: %w", v, err)
		}
		cfg.Server.Port = n
	}
	if v := os.Getenv("SM_BASE_URL"); v != "" {
		cfg.Server.BaseURL = v
	} else {
		cfg.Server.BaseURL = fmt.Sprintf("http://localhost:%d", cfg.Server.Port)
	}
	if v := os.Getenv("SM_SECRET"); v != "" {
		cfg.Server.Secret = v
	} else if err := ensureSecret(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ensureSecret loads (or generates and persists) the session secret from a file
// next to the DB, so it stays stable across restarts without any config.
func ensureSecret(cfg *Config) error {
	dir := filepath.Dir(cfg.DB.Path)
	p := filepath.Join(dir, "secret")
	if b, err := os.ReadFile(p); err == nil { // #nosec G304 -- path derived from admin's own SM_DB
		if s := strings.TrimSpace(string(bytes.TrimSpace(b))); s != "" {
			cfg.Server.Secret = s
			return nil
		}
	}
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("config: generate secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("config: create data dir: %w", err)
	}
	if err := os.WriteFile(p, []byte(secret), 0o600); err != nil {
		return fmt.Errorf("config: persist secret: %w", err)
	}
	slog.Info("generated session secret", "path", p)
	cfg.Server.Secret = secret
	return nil
}

// Interval and MaxMessagesPerSync defaults now live in the store (Prefs); kept
// here only as a shared fallback constant for the mail engine.
const (
	DefaultPollInterval = 5 * time.Minute
	DefaultMaxPerSync   = 500
	// DefaultBodyCache is how many of the newest inbox messages per account the
	// warmer prefetches bodies for and the body cache keeps — one full message
	// list page (server.listLimit), i.e. what the user can actually see and
	// plausibly click next. Anything older is fetched on open and cached then.
	DefaultBodyCache = 200
)
