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
	HideAvatarMobile bool              // hide sender avatar/favicon on mobile only (default false)
	DarkMessages     bool              // open message bodies in dark mode by default (default false)
	RememberMsgTheme bool              // remember the light/dark choice per message (default false)
	SyncMonths       int               // how far back to download on first sync; 0 = all (count-capped)
	MaxPerSync       int               // max messages to fetch per account per poll cycle (default 500)
	AccountColors    map[string]string // account name -> hex color, e.g. "#6366f1"
	QuickActions     string            // comma-separated ids of optional message actions to show; see AllQuickActions
	SearchScope      string            // topbar search default scope: "all", "account", or "folder" (default "all")
	ComposeMode      string            // compose editor mode: "plain", "html", or "markdown" (default "html")
	UndoSendDelay    int               // seconds Send waits before delivering, undo-able (3|5|10, default 5)
}

// AllQuickActions lists every message action the user can place in the action
// bar, in the "⋯" menu, or hide entirely — in their default display order.
var AllQuickActions = []struct{ ID, Label string }{
	{"reply", "Reply"},
	{"unread", "Mark read / unread"},
	{"archive", "Archive"},
	{"replyall", "Reply all"},
	{"forward", "Forward"},
	{"star", "Star / unstar"},
	{"dark", "Toggle dark/light message"},
	{"translate", "Translate"},
	{"refetch", "Re-fetch from server"},
	{"spam", "Mark as spam"},
	{"delete", "Delete"},
}

// defaultQuickActions: reply / mark-unread / archive directly in the bar,
// everything else one click away in the "⋯" menu.
func defaultQuickActions() string {
	bar := []string{"reply", "unread", "archive"}
	var menu []string
	for _, a := range AllQuickActions {
		if a.ID != "reply" && a.ID != "unread" && a.ID != "archive" {
			menu = append(menu, a.ID)
		}
	}
	return JoinQuickActions(bar, menu)
}

// SplitQuickActions parses the ordered "id=bar,id=menu,..." preference into
// the two placement lists (ids absent from the string are hidden). Legacy
// pre-placement values ("id,id,...") get the original fixed bar plus the
// stored ids as the menu.
func SplitQuickActions(pref string) (bar, menu []string) {
	known := map[string]bool{}
	for _, a := range AllQuickActions {
		known[a.ID] = true
	}
	legacy := pref != "" && !strings.Contains(pref, "=")
	seen := map[string]bool{"": true}
	if legacy {
		bar = []string{"reply", "unread", "archive"}
		for _, id := range bar {
			seen[id] = true
		}
	}
	for _, e := range strings.Split(pref, ",") {
		id, place, _ := strings.Cut(e, "=")
		if !known[id] || seen[id] {
			continue
		}
		seen[id] = true
		if legacy || place == "menu" {
			menu = append(menu, id)
		} else if place == "bar" {
			bar = append(bar, id)
		}
	}
	return bar, menu
}

// JoinQuickActions is the inverse of SplitQuickActions.
func JoinQuickActions(bar, menu []string) string {
	parts := make([]string, 0, len(bar)+len(menu))
	for _, id := range bar {
		parts = append(parts, id+"=bar")
	}
	for _, id := range menu {
		parts = append(parts, id+"=menu")
	}
	return strings.Join(parts, ",")
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
		HideAvatarMobile: false,
		DarkMessages:     false,
		RememberMsgTheme: false,
		SyncMonths:       0,
		MaxPerSync:       500,
		AccountColors:    map[string]string{},
		QuickActions:     defaultQuickActions(),
		SearchScope:      "all",
		ComposeMode:      "html",
		UndoSendDelay:    5,
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
	if v, ok := s.getSetting("hide_avatar_mobile"); ok {
		p.HideAvatarMobile = v == "1"
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
	if v, ok := s.getSetting("max_per_sync"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.MaxPerSync = n
		}
	}
	if v, ok := s.getSetting("quick_actions"); ok {
		p.QuickActions = v
	}
	if v, ok := s.getSetting("search_scope"); ok && (v == "all" || v == "account" || v == "folder") {
		p.SearchScope = v
	}
	if v, ok := s.getSetting("compose_mode"); ok && (v == "plain" || v == "html" || v == "markdown") {
		p.ComposeMode = v
	}
	if v, ok := s.getSetting("undo_send_delay"); ok {
		if n, err := strconv.Atoi(v); err == nil && (n == 3 || n == 5 || n == 10) {
			p.UndoSendDelay = n
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
		"hide_avatar_mobile": boolStr(p.HideAvatarMobile),
		"dark_messages":      boolStr(p.DarkMessages),
		"remember_msg_theme": boolStr(p.RememberMsgTheme),
		"sync_months":        strconv.Itoa(p.SyncMonths),
		"max_per_sync":       strconv.Itoa(p.MaxPerSync),
		"quick_actions":      p.QuickActions,
		"search_scope":       p.SearchScope,
		"compose_mode":       p.ComposeMode,
		"undo_send_delay":    strconv.Itoa(p.UndoSendDelay),
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

// AppConfig holds the integration credentials that used to live in config.toml's
// [translate] and [ai] sections. Kept out of Prefs because Prefs is handed to
// templates and these values are secret.
type AppConfig struct {
	TranslateAPIKey string
	TranslateTarget string // ISO code, default "en"
	AIKey           string // OpenRouter API key
	AIModel         string // default "anthropic/claude-sonnet-4-6"
	AITone          string // professional|neutral|friendly|casual, default neutral
	AIBrevity       string // concise|normal|detailed, default normal
	AIReplyOptions  int    // reply directions to generate (2-5), default 3
	AILanguage      string // "auto" or a fixed language name, default auto
}

func (s *Store) GetAppConfig() AppConfig {
	c := AppConfig{TranslateTarget: "en", AIModel: "anthropic/claude-sonnet-4-6",
		AITone: "neutral", AIBrevity: "normal", AIReplyOptions: 3, AILanguage: "auto"}
	if v, ok := s.getSetting("translate_api_key"); ok {
		c.TranslateAPIKey = v
	}
	if v, ok := s.getSetting("translate_target"); ok && v != "" {
		c.TranslateTarget = v
	}
	if v, ok := s.getSetting("ai_openrouter_key"); ok {
		c.AIKey = v
	}
	if v, ok := s.getSetting("ai_model"); ok && v != "" {
		c.AIModel = v
	}
	if v, ok := s.getSetting("ai_tone"); ok && v != "" {
		c.AITone = v
	}
	if v, ok := s.getSetting("ai_brevity"); ok && v != "" {
		c.AIBrevity = v
	}
	if v, ok := s.getSetting("ai_reply_options"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 2 && n <= 5 {
			c.AIReplyOptions = n
		}
	}
	if v, ok := s.getSetting("ai_language"); ok && v != "" {
		c.AILanguage = v
	}
	return c
}

func (s *Store) SaveAppConfig(c AppConfig) error {
	if c.TranslateTarget == "" {
		c.TranslateTarget = "en"
	}
	if c.AIModel == "" {
		c.AIModel = "anthropic/claude-sonnet-4-6"
	}
	if c.AITone == "" {
		c.AITone = "neutral"
	}
	if c.AIBrevity == "" {
		c.AIBrevity = "normal"
	}
	if c.AIReplyOptions < 2 || c.AIReplyOptions > 5 {
		c.AIReplyOptions = 3
	}
	if c.AILanguage == "" {
		c.AILanguage = "auto"
	}
	kv := map[string]string{
		"translate_api_key": c.TranslateAPIKey,
		"translate_target":  c.TranslateTarget,
		"ai_openrouter_key": c.AIKey,
		"ai_model":          c.AIModel,
		"ai_tone":           c.AITone,
		"ai_brevity":        c.AIBrevity,
		"ai_reply_options":  strconv.Itoa(c.AIReplyOptions),
		"ai_language":       c.AILanguage,
	}
	for k, v := range kv {
		if err := s.setSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}
