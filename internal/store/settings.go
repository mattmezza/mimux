package store

import (
	"database/sql"
	"log/slog"
	"strconv"
	"strings"
)

// Prefs holds user-tunable app preferences. Zero-config defaults live in
// defaultPrefs(); missing keys fall back to those.
type Prefs struct {
	MarkReadDelay    int               // seconds; 0 = immediately on open
	SyncIntervalMin  int               // minutes between syncs (default 5)
	PreviewLines     int               // list snippet lines 0..3 (default 1)
	ShowAvatar       bool              // show sender avatar (default true)
	ShowAccountBadge bool              // show account-name badge on list rows (default true)
	ShowAttachMarker bool              // show attachment marker on list rows (default true)
	ShowFavicon      bool              // use sender-domain favicon as avatar (default false)
	DarkMessages     bool              // open message bodies in dark mode by default (default false)
	RememberMsgTheme bool              // remember the light/dark choice per message (default false)
	SyncMonths       int               // how far back to download on first sync; 0 = all (count-capped)
	AccountColors    map[string]string // account name -> hex color, e.g. "#6366f1"
}

func defaultPrefs() Prefs {
	return Prefs{
		MarkReadDelay:    0,
		SyncIntervalMin:  5,
		PreviewLines:     1,
		ShowAvatar:       true,
		ShowAccountBadge: true,
		ShowAttachMarker: true,
		ShowFavicon:      false,
		DarkMessages:     false,
		RememberMsgTheme: false,
		SyncMonths:       0,
		AccountColors:    map[string]string{},
	}
}

const accountColorPrefix = "account_color:"

// getSetting returns a stored value, ok=false on miss.
func (s *Store) getSetting(key string) (string, bool) {
	var v string
	err := s.DB.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		slog.Error("getSetting", "key", key, "err", err)
		return "", false
	}
	return v, true
}

func (s *Store) setSetting(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO app_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// GetPrefs returns stored preferences, falling back to defaults for any missing
// or unparseable key. Never errors out the caller.
func (s *Store) GetPrefs() Prefs {
	p := defaultPrefs()
	if v, ok := s.getSetting("mark_read_delay"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			p.MarkReadDelay = n
		}
	}
	if v, ok := s.getSetting("sync_interval_min"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			p.SyncIntervalMin = n
		}
	}
	if v, ok := s.getSetting("preview_lines"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			p.PreviewLines = n
		}
	}
	if v, ok := s.getSetting("show_avatar"); ok {
		p.ShowAvatar = v == "1"
	}
	if v, ok := s.getSetting("show_account_badge"); ok {
		p.ShowAccountBadge = v == "1"
	}
	if v, ok := s.getSetting("show_attach_marker"); ok {
		p.ShowAttachMarker = v == "1"
	}
	if v, ok := s.getSetting("show_favicon"); ok {
		p.ShowFavicon = v == "1"
	}
	if v, ok := s.getSetting("dark_messages"); ok {
		p.DarkMessages = v == "1"
	}
	if v, ok := s.getSetting("remember_msg_theme"); ok {
		p.RememberMsgTheme = v == "1"
	}
	if v, ok := s.getSetting("sync_months"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			p.SyncMonths = n
		}
	}
	rows, err := s.DB.Query(`SELECT key, value FROM app_settings WHERE key LIKE ?`, accountColorPrefix+"%")
	if err != nil {
		slog.Error("GetPrefs account colors", "err", err)
		return p
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			continue
		}
		p.AccountColors[strings.TrimPrefix(k, accountColorPrefix)] = v
	}
	return p
}

// SavePrefs writes all preference keys.
func (s *Store) SavePrefs(p Prefs) error {
	kv := map[string]string{
		"mark_read_delay":    strconv.Itoa(p.MarkReadDelay),
		"sync_interval_min":  strconv.Itoa(p.SyncIntervalMin),
		"preview_lines":      strconv.Itoa(p.PreviewLines),
		"show_avatar":        boolStr(p.ShowAvatar),
		"show_account_badge": boolStr(p.ShowAccountBadge),
		"show_attach_marker": boolStr(p.ShowAttachMarker),
		"show_favicon":       boolStr(p.ShowFavicon),
		"dark_messages":      boolStr(p.DarkMessages),
		"remember_msg_theme": boolStr(p.RememberMsgTheme),
		"sync_months":        strconv.Itoa(p.SyncMonths),
	}
	for name, color := range p.AccountColors {
		kv[accountColorPrefix+name] = color
	}
	for k, v := range kv {
		if err := s.setSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
