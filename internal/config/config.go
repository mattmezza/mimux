// Package config parses the TOML config file. Path comes from SM_CONFIG
// env var or the -config flag; everything else is config-file-driven.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/mattmezza/sm/internal/account"
)

type Config struct {
	Server    Server    `toml:"server"`
	DB        DB        `toml:"db"`
	Accounts  []Account `toml:"accounts"`
	Translate Translate `toml:"translate"`
	AI        AI        `toml:"ai"`
	Sync      Sync      `toml:"sync"`
}

type Server struct {
	Host    string `toml:"host"`
	Port    int    `toml:"port"`
	BaseURL string `toml:"base_url"`
	Secret  string `toml:"secret"`
}

type DB struct {
	Path string `toml:"path"`
}

type Account struct {
	// Name is the account's identity/key (folders, tokens, colors, filters are
	// keyed by it) and the badge label. Comes from `account_name`; falls back to
	// the legacy `name` when unset (see Load).
	Name string `toml:"account_name"`
	// SenderName is the display name on outgoing mail (the From header). Comes
	// from `name`; falls back to Name when unset.
	SenderName         string  `toml:"name"`
	Provider           string  `toml:"provider"`
	Email              string  `toml:"email"`
	Aliases            []Alias `toml:"aliases"` // extra identities this account can send/receive as
	Auth               string  `toml:"auth"`    // "password" | "oauth2"
	Password           string  `toml:"password"`
	OAuth2ClientID     string  `toml:"oauth2_client_id"`
	OAuth2ClientSecret string  `toml:"oauth2_client_secret"`
	IMAPHost           string  `toml:"imap_host"`
	IMAPPort           int     `toml:"imap_port"`
	SMTPHost           string  `toml:"smtp_host"`
	SMTPPort           int     `toml:"smtp_port"`
}

// Alias is an extra send/receive identity on an account, with its own sender
// display name and address.
type Alias struct {
	Name  string `toml:"name"`  // display name on outgoing mail sent as this alias
	Email string `toml:"email"` // the alias address
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

type Translate struct {
	APIKey         string `toml:"api_key"`
	TargetLanguage string `toml:"target_language"`
}

type AI struct {
	OpenRouterAPIKey string `toml:"openrouter_api_key"`
	Model            string `toml:"model"`
}

type Sync struct {
	PollInterval       duration `toml:"poll_interval"`
	MaxMessagesPerSync int      `toml:"max_messages_per_sync"`
}

// duration lets TOML hold "5m"-style strings.
type duration time.Duration

func (d *duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	*d = duration(v)
	return err
}

func (s Sync) Interval() time.Duration { return time.Duration(s.PollInterval) }

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server:    Server{Host: "0.0.0.0", Port: 8080},
		DB:        DB{Path: "./data/sm.db"},
		Translate: Translate{TargetLanguage: "en"},
		AI:        AI{Model: "anthropic/claude-sonnet-4-6"},
		Sync:      Sync{PollInterval: duration(5 * time.Minute), MaxMessagesPerSync: 500},
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if cfg.Server.Secret == "" {
		return nil, errors.New("config: server.secret is required")
	}
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		// Back-compat: `name` used to be the identity. When `account_name` is
		// unset, keep the old behavior (name is both identity and sender name);
		// otherwise account_name is the identity and name is the sender name.
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
			return nil, fmt.Errorf("config: account %q: no provider preset and no imap/smtp host", a.Name)
		}
		if a.Auth == "" {
			a.Auth = "password"
		}
	}
	return cfg, nil
}
