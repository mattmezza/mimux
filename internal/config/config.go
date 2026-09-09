// SPDX-License-Identifier: AGPL-3.0-only
// Package config holds bootstrap config only — the settings that cannot live in
// the database (chicken-and-egg): db path, listen host/port, public base_url and
// the session secret. Everything else (accounts, sync cadence, translate/AI
// keys) lives in the DB and is edited from the Settings GUI.
//
// Bootstrap comes entirely from environment variables, each with a sane default,
// so a fresh install with ZERO env vars boots and works:
//
//	MIMUX_DB       sqlite path            (default ./data/mimux.db)
//	MIMUX_HOST     bind address           (default 0.0.0.0)
//	MIMUX_PORT     port                   (default 8083)
//	MIMUX_BASE_URL public URL             (default http://localhost:<port>)
//	MIMUX_SECRET   session/CSRF secret    (default: generated once, persisted next
//	                                        to the DB so sessions survive restarts)
//	MIMUX_API_RATE_LIMIT  API requests per token per minute (default 120, 0 = off)
//	MIMUX_LICENCE_KEY     pro licence key; takes precedence over the one saved
//	                        in Settings → Licence (free builds ignore it)
//
// The pre-rename SM_* names still work for one release — see Env.
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

	"github.com/mattmezza/mimux/internal/account"
)

type Config struct {
	Server Server
	DB     DB
	API    API
	// Version is the running build's version string, set by cmd/mimux at boot
	// (it owns the -ldflags value). It rides here because the pro layer's
	// licence check compares it against a perpetual licence's watermark, and
	// cfg is what it already gets through ext.Deps.
	Version string
	// BuildDate is when this build was released, set by cmd/mimux at boot from
	// its own -ldflags value. RFC3339 or a bare 2006-01-02, and empty for any
	// build nobody released — a local `go build`, `make build`, `go run`. A
	// perpetual licence covers every build released before its coverage ends,
	// so this is the date that check compares against; empty fails open.
	BuildDate string
	// LicenceKey is MIMUX_LICENCE_KEY. Blank falls back to the key stored in
	// the database. Bootstrap config like the rest of this struct: an install
	// that provisions its licence from the environment should not have to reach
	// into SQLite to do it.
	LicenceKey string
	// Accounts is a runtime snapshot of the DB-backed accounts, refreshed by the
	// server whenever the account list changes. It is NOT parsed from any file —
	// it exists so the HTTP layer and compose selector can read the current
	// accounts through the existing cfg without per-request DB plumbing.
	Accounts []Account
}

type Server struct {
	Host            string
	Port            int
	BaseURL         string
	BaseURLExplicit bool // true when MIMUX_BASE_URL was supplied
	Secret          string
}

type DB struct {
	Path string
}

// API holds the machine-facing surface's bootstrap knobs. The endpoints are the
// pro layer's (see pro/), but their configuration is ordinary bootstrap config
// and lives here with the rest — the free build simply has nothing reading it.
type API struct {
	// RateLimitPerMinute is the per-token request budget. 0 disables limiting.
	RateLimitPerMinute int
}

// DefaultAPIRateLimit is the per-token request budget when MIMUX_API_RATE_LIMIT
// is unset: generous for a single user's own automation, low enough that a
// runaway script cannot pin the mailbox.
const DefaultAPIRateLimit = 120

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
// The json tags are load-bearing: this struct is marshalled into the Edit
// button's data-acct payload, and the form prefill reads name/email. Untagged
// it serialized as Name/Email and every alias row came up blank. Decoding stays
// compatible with rows written before the tags — encoding/json matches field
// names case-insensitively.
type Alias struct {
	Name  string `json:"name"`  // display name on outgoing mail sent as this alias
	Email string `json:"email"` // the alias address
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

// Env reads the MIMUX_<key> environment variable, falling back to the
// pre-rename SM_<key> with a one-line deprecation warning.
// TODO(v0.21): drop SM_ fallback.
func Env(key string) string {
	if v := os.Getenv("MIMUX_" + key); v != "" {
		return v
	}
	if v := os.Getenv("SM_" + key); v != "" {
		slog.Warn("config: deprecated env var, rename it", "old", "SM_"+key, "new", "MIMUX_"+key)
		return v
	}
	return ""
}

// Load builds the bootstrap config from environment variables + defaults,
// generating and persisting a session secret when MIMUX_SECRET is unset.
func Load() (*Config, error) {
	cfg := &Config{
		Server: Server{Host: "0.0.0.0", Port: 8083},
		DB:     DB{Path: "./data/mimux.db"},
		API:    API{RateLimitPerMinute: DefaultAPIRateLimit},
	}
	if v := Env("API_RATE_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("config: MIMUX_API_RATE_LIMIT %q: want a non-negative integer", v)
		}
		cfg.API.RateLimitPerMinute = n
	}
	cfg.LicenceKey = strings.TrimSpace(Env("LICENCE_KEY"))
	if v := Env("DB"); v != "" {
		cfg.DB.Path = v
	}
	if v := Env("HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := Env("PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config: MIMUX_PORT %q: %w", v, err)
		}
		cfg.Server.Port = n
	}
	if v := Env("BASE_URL"); v != "" {
		cfg.Server.BaseURL = v
		cfg.Server.BaseURLExplicit = true
	} else {
		cfg.Server.BaseURL = fmt.Sprintf("http://localhost:%d", cfg.Server.Port)
	}
	if v := Env("SECRET"); v != "" {
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
	if b, err := os.ReadFile(p); err == nil { // #nosec G304 -- path derived from admin's own MIMUX_DB
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

// BareEmail strips a "Name <addr>" wrapper down to the address.
func BareEmail(a string) string {
	if i := strings.LastIndex(a, "<"); i >= 0 {
		return strings.TrimSpace(strings.TrimSuffix(a[i+1:], ">"))
	}
	return strings.TrimSpace(a)
}

// AccountForAddress finds the account that owns a from-address — its primary
// Email or one of its aliases — matched case-insensitively on the bare
// address. Moved down from internal/server so compose routing and the pro
// API's send endpoint resolve identities identically.
func AccountForAddress(accounts []Account, addr string) (Account, bool) {
	want := strings.ToLower(BareEmail(addr))
	if want == "" {
		return Account{}, false
	}
	for _, a := range accounts {
		if strings.ToLower(a.Email) == want {
			return a, true
		}
		for _, al := range a.Aliases {
			if strings.ToLower(BareEmail(al.Email)) == want {
				return a, true
			}
		}
	}
	return Account{}, false
}
