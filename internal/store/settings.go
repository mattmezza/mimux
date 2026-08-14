package store

import (
	"database/sql"
	"hash/fnv"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/mattmezza/sm/internal/config"
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
	ShowListLabels   bool              // show labels on message-list rows (default false)
	ShowFavicon      bool              // use sender-domain favicon as avatar (default false)
	HideAvatarMobile bool              // hide sender avatar/favicon on mobile only (default false)
	AvatarShape      string            // sender-avatar corner style: circle|rounded|square (default circle)
	DarkMessages     bool              // open message bodies in dark mode by default (default false)
	RememberMsgTheme bool              // remember the light/dark choice per message (default false)
	SyncMonths       int               // how far back to download on first sync; 0 = all (count-capped)
	MaxPerSync       int               // max messages to fetch per account per poll cycle (default 500)
	BodyCache        int               // newest inbox messages per account to keep bodies downloaded for; 0 = off (default 200)
	AccountColors    map[string]string // account name -> hex color, e.g. "#6366f1"
	QuickActions     string            // comma-separated ids of optional message actions to show; see AllQuickActions
	SearchScope      string            // topbar search default scope: "all", "account", or "folder" (default "all")
	ComposeMode      string            // compose editor mode: "plain", "html", or "markdown" (default "html")
	ComposeLayout    string            // window layout for new messages: "fullscreen", "popup", or "modal" (default "fullscreen")
	ReplyLayout      string            // window layout for reply/reply-all/forward: "fullscreen", "popup", or "modal" (default "popup")
	UndoSendDelay    int               // seconds Send waits before delivering, undo-able (3|5|10, default 5)
	ThreadOrder      string            // message order inside an open thread: "oldest" (newest at the bottom, default) or "newest"
	RowDoubleAction  string            // double-click / double-tap a list row; see AllRowActions (default "unread")
	SwipeLeftAction  string            // swipe a list row left; see AllRowActions (default "none")
	SwipeRightAction string            // swipe a list row right; see AllRowActions (default "unread")
	// NotifyScope is the master switch for notifications (Settings →
	// Notifications): "off" (default — nothing is ever sent, and no permission
	// prompt is ever shown), "rules" (only messages matched by a filter rule
	// with the "notify" action) or "all" (every new inbox message). It gates
	// both transports; each transport is separately configured (a Web Push
	// subscription per device, an ntfy topic URL below).
	NotifyScope string
	// NtfyURL is a full ntfy topic URL (https://ntfy.sh/<topic> or a
	// self-hosted one). Blank disables that transport. It needs no browser
	// permission and no installed PWA — the fallback for a phone that can't or
	// won't run Web Push.
	NtfyURL string
}

// AllNotifyScopes are the notification master-switch choices, for the Settings
// select. Order matters: it's the order shown.
var AllNotifyScopes = []struct{ ID, Label string }{
	{"off", "Off — never notify me"},
	{"rules", "Only what my filter rules say"},
	{"all", "Every new message in an inbox"},
}

// ValidNotifyScope returns v if it names a scope, else def.
func ValidNotifyScope(v, def string) string {
	for _, s := range AllNotifyScopes {
		if s.ID == v {
			return v
		}
	}
	return def
}

// AllRowActions lists what a gesture on a message list row can do — same menu
// for the double-click/double-tap and for each swipe direction.
var AllRowActions = []struct{ ID, Label string }{
	{"unread", "Mark read / unread"},
	{"star", "Star / unstar"},
	{"archive", "Archive"},
	{"delete", "Delete"},
	{"open", "Open message"},
	{"none", "Do nothing"},
}

// ValidRowAction returns v if it names a row action, else def.
func ValidRowAction(v, def string) string {
	for _, a := range AllRowActions {
		if a.ID == v {
			return v
		}
	}
	return def
}

// AllSummaryLevels lists the AI summary detail levels — the Settings default
// and the segmented control on the summary strip pick from the same list.
var AllSummaryLevels = []struct{ ID, Label string }{
	{"oneline", "One line"},
	{"brief", "Brief"},
	{"detailed", "Detailed"},
}

// ValidSummaryLevel returns v if it names a summary level, else def.
func ValidSummaryLevel(v, def string) string {
	for _, l := range AllSummaryLevels {
		if l.ID == v {
			return v
		}
	}
	return def
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
	{"summarize", "Summarize (AI)"},
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
		ShowListLabels:   false,
		ShowFavicon:      false,
		HideAvatarMobile: false,
		AvatarShape:      "circle",
		DarkMessages:     false,
		RememberMsgTheme: false,
		SyncMonths:       0,
		MaxPerSync:       500,
		BodyCache:        config.DefaultBodyCache,
		AccountColors:    map[string]string{},
		QuickActions:     defaultQuickActions(),
		SearchScope:      "all",
		ComposeMode:      "html",
		ComposeLayout:    "fullscreen",
		ReplyLayout:      "popup",
		UndoSendDelay:    5,
		ThreadOrder:      "oldest",
		RowDoubleAction:  "unread",
		SwipeLeftAction:  "none",
		SwipeRightAction: "unread",
		NotifyScope:      "off",
		NtfyURL:          "",
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
	if v, ok := s.getSetting("show_list_labels"); ok {
		p.ShowListLabels = v == "1"
	}
	if v, ok := s.getSetting("show_favicon"); ok {
		p.ShowFavicon = v == "1"
	}
	if v, ok := s.getSetting("hide_avatar_mobile"); ok {
		p.HideAvatarMobile = v == "1"
	}
	if v, ok := s.getSetting("avatar_shape"); ok && (v == "circle" || v == "rounded" || v == "square") {
		p.AvatarShape = v
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
	if v, ok := s.getSetting("body_cache"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			p.BodyCache = n
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
	if v, ok := s.getSetting("compose_layout"); ok && (v == "fullscreen" || v == "popup" || v == "modal") {
		p.ComposeLayout = v
	}
	if v, ok := s.getSetting("reply_layout"); ok && (v == "fullscreen" || v == "popup" || v == "modal") {
		p.ReplyLayout = v
	}
	if v, ok := s.getSetting("thread_order"); ok && (v == "oldest" || v == "newest") {
		p.ThreadOrder = v
	}
	if v, ok := s.getSetting("row_double_action"); ok {
		p.RowDoubleAction = ValidRowAction(v, p.RowDoubleAction)
	}
	if v, ok := s.getSetting("swipe_left_action"); ok {
		p.SwipeLeftAction = ValidRowAction(v, p.SwipeLeftAction)
	}
	if v, ok := s.getSetting("swipe_right_action"); ok {
		p.SwipeRightAction = ValidRowAction(v, p.SwipeRightAction)
	}
	if v, ok := s.getSetting("notify_scope"); ok {
		p.NotifyScope = ValidNotifyScope(v, p.NotifyScope)
	}
	if v, ok := s.getSetting("ntfy_url"); ok {
		p.NtfyURL = v
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
		"show_list_labels":   boolStr(p.ShowListLabels),
		"show_favicon":       boolStr(p.ShowFavicon),
		"hide_avatar_mobile": boolStr(p.HideAvatarMobile),
		"avatar_shape":       p.AvatarShape,
		"dark_messages":      boolStr(p.DarkMessages),
		"remember_msg_theme": boolStr(p.RememberMsgTheme),
		"sync_months":        strconv.Itoa(p.SyncMonths),
		"max_per_sync":       strconv.Itoa(p.MaxPerSync),
		"body_cache":         strconv.Itoa(p.BodyCache),
		"quick_actions":      p.QuickActions,
		"search_scope":       p.SearchScope,
		"compose_mode":       p.ComposeMode,
		"compose_layout":     p.ComposeLayout,
		"reply_layout":       p.ReplyLayout,
		"undo_send_delay":    strconv.Itoa(p.UndoSendDelay),
		"thread_order":       p.ThreadOrder,
		"row_double_action":  p.RowDoubleAction,
		"swipe_left_action":  p.SwipeLeftAction,
		"swipe_right_action": p.SwipeRightAction,
		"notify_scope":       p.NotifyScope,
		"ntfy_url":           p.NtfyURL,
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

// AIFeature names one of the four AI features. Each one can pin its own model;
// everything else it needs lives in the matching AppConfig field.
type AIFeature string

const (
	AICompose   AIFeature = "compose"   // writes a compose/reply draft
	AIOptions   AIFeature = "options"   // suggests reply directions
	AIRefine    AIFeature = "refine"    // rewrites an existing draft
	AISummarize AIFeature = "summarize" // condenses a message
)

// defaultAIModel is what every feature runs on until the user says otherwise.
const defaultAIModel = "anthropic/claude-sonnet-4-6"

// AppConfig holds the integration credentials that used to live in config.toml's
// [translate] and [ai] sections. Kept out of Prefs because Prefs is handed to
// templates and these values are secret.
type AppConfig struct {
	TranslateAPIKey string
	TranslateTarget string // ISO code, default "en"
	AIKey           string // OpenRouter API key — one account, shared by every feature
	AIModel         string // default model, default "anthropic/claude-sonnet-4-6"
	// Per-feature model overrides. Blank inherits AIModel — same
	// inherit-or-global shape as the per-account sync overrides.
	AIComposeModel   string
	AIOptionsModel   string
	AIRefineModel    string
	AISummarizeModel string
	// Tone/brevity/language are shared: they describe the user's voice, not one
	// feature, and refine's own action ("more formal", "warmer") already carries
	// its per-call direction.
	// NOTE: shared, not duplicated per feature — split them into
	// ai_<feature>_tone/brevity/language if someone actually wants a formal
	// draft and a casual rewrite.
	AITone         string // professional|neutral|friendly|casual, default neutral (compose + refine)
	AIBrevity      string // concise|normal|detailed, default normal (compose + refine)
	AILanguage     string // "auto" or a fixed language name, default auto (compose + refine + summarize)
	AIReplyOptions int    // reply directions to generate (2-5), default 3 (options only)
	AISummaryLevel string // default detail level for Summarize; see AllSummaryLevels
	// Look: the UI accent and the app icon. Not secret, but they live here
	// rather than in Prefs because the icon endpoint reads them on every page
	// render and this is the smaller struct.
	Accent     string // an AllAccents id, or a "#rrggbb" custom colour; default "indigo"
	IconBG     string // icon background, "#rrggbb" or "transparent"; default "#18181b"
	IconAccent string // icon envelope colour; blank inherits the accent's 500 step
	IconLeaf   string // icon leaf colour; default "#4ade80"
	IconShape  string // icon corner rounding: rounded|square|circle; default "rounded"
}

// AllAccents are the named UI accents. Each carries the Tailwind steps the
// markup actually uses — 200/300/400/500/600/900/950, plus 700 for the light
// theme's darkened text steps — verified against dist.css.
// NOTE: literal hex, not a generated palette. Tailwind v4 only emits
// variables for scales the markup references, so a computed scale would
// silently resolve to nothing.
var AllAccents = []struct {
	ID, Label string
	Steps     [8]string // 200,300,400,500,600,700,900,950
}{
	{"indigo", "Indigo", [8]string{"#c7d2fe", "#a5b4fc", "#818cf8", "#6366f1", "#4f46e5", "#4338ca", "#312e81", "#1e1b4b"}},
	{"violet", "Violet", [8]string{"#ddd6fe", "#c4b5fd", "#a78bfa", "#8b5cf6", "#7c3aed", "#6d28d9", "#4c1d95", "#2e1065"}},
	{"sky", "Sky", [8]string{"#bae6fd", "#7dd3fc", "#38bdf8", "#0ea5e9", "#0284c7", "#0369a1", "#0c4a6e", "#082f49"}},
	{"emerald", "Emerald", [8]string{"#a7f3d0", "#6ee7b7", "#34d399", "#10b981", "#059669", "#047857", "#064e3b", "#022c22"}},
	{"amber", "Amber", [8]string{"#fde68a", "#fcd34d", "#fbbf24", "#f59e0b", "#d97706", "#b45309", "#78350f", "#451a03"}},
	{"rose", "Rose", [8]string{"#fecdd3", "#fda4af", "#fb7185", "#f43f5e", "#e11d48", "#be123c", "#881337", "#4c0519"}},
}

// AccentSteps returns the scale for a named accent; ok=false means it is a
// custom hex and the caller derives its own ramp.
func AccentSteps(id string) ([8]string, bool) {
	for _, a := range AllAccents {
		if a.ID == id {
			return a.Steps, true
		}
	}
	return [8]string{}, false
}

// ValidHexColor returns c when it is a "#rrggbb" colour, else def. These values
// are interpolated straight into the icon SVG and into a <style> block, so
// nothing gets past this gate — not from the form, not from the query string.
func ValidHexColor(c, def string) string {
	if len(c) != 7 || c[0] != '#' {
		return def
	}
	for i := 1; i < 7; i++ {
		d := c[i]
		if (d < '0' || d > '9') && (d < 'a' || d > 'f') && (d < 'A' || d > 'F') {
			return def
		}
	}
	return c
}

// ValidIconBG is ValidHexColor plus the "transparent" sentinel, which some
// launchers want (they paint their own plate behind the mark).
func ValidIconBG(c, def string) string {
	if c == "transparent" {
		return c
	}
	return ValidHexColor(c, def)
}

// ValidAccent returns v when it names an accent or is a custom hex, else def.
func ValidAccent(v, def string) string {
	if _, ok := AccentSteps(v); ok {
		return v
	}
	return ValidHexColor(v, def)
}

// IconVersion is a short hash of everything that changes the icon's pixels. It
// rides the icon URL as ?v=, so a colour change mints a new URL and neither the
// browser's favicon cache nor the service worker's runtime cache can serve the
// old mark.
func (c AppConfig) IconVersion() string {
	h := fnv.New32a()
	_, _ = io.WriteString(h, c.Accent+c.IconBG+c.IconAccent+c.IconLeaf+c.IconShape)
	return strconv.FormatUint(uint64(h.Sum32()), 36)
}

// ModelFor resolves the model one feature runs on: its own override where set,
// else the default model. Mirrors EffectiveSyncSettings.
// NOTE: four flat fields and a switch, not a feature registry — there are
// four features and they are listed right here.
func (c AppConfig) ModelFor(f AIFeature) string {
	var override string
	switch f {
	case AICompose:
		override = c.AIComposeModel
	case AIOptions:
		override = c.AIOptionsModel
	case AIRefine:
		override = c.AIRefineModel
	case AISummarize:
		override = c.AISummarizeModel
	}
	if override != "" {
		return override
	}
	return c.AIModel
}

func (s *Store) GetAppConfig() AppConfig {
	c := AppConfig{TranslateTarget: "en", AIModel: defaultAIModel,
		AITone: "neutral", AIBrevity: "normal", AIReplyOptions: 3, AILanguage: "auto",
		AISummaryLevel: "brief",
		Accent:         "indigo", IconBG: "#18181b", IconLeaf: "#4ade80", IconShape: "rounded"}
	if v, ok := s.getSetting("translate_api_key"); ok {
		c.TranslateAPIKey = v
	}
	if v, ok := s.getSetting("translate_target"); ok && v != "" {
		c.TranslateTarget = v
	}
	if v, ok := s.getSetting("ai_openrouter_key"); ok {
		c.AIKey = v
	}
	// ai_model stays the default model, so an install that configured it before
	// the per-feature split keeps running on it everywhere.
	if v, ok := s.getSetting("ai_model"); ok && v != "" {
		c.AIModel = v
	}
	c.AIComposeModel, _ = s.getSetting("ai_compose_model")
	c.AIOptionsModel, _ = s.getSetting("ai_options_model")
	c.AIRefineModel, _ = s.getSetting("ai_refine_model")
	c.AISummarizeModel, _ = s.getSetting("ai_summarize_model")
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
	if v, ok := s.getSetting("ai_summary_level"); ok {
		c.AISummaryLevel = ValidSummaryLevel(v, c.AISummaryLevel)
	}
	// Look. Re-validated on read as well as on write: these end up inside an
	// SVG and a <style> block, and a hand-edited DB row is still untrusted.
	if v, ok := s.getSetting("ui_accent"); ok {
		c.Accent = ValidAccent(v, c.Accent)
	}
	if v, ok := s.getSetting("icon_bg"); ok {
		c.IconBG = ValidIconBG(v, c.IconBG)
	}
	if v, ok := s.getSetting("icon_accent"); ok && v != "" {
		c.IconAccent = ValidHexColor(v, "")
	}
	if v, ok := s.getSetting("icon_leaf"); ok {
		c.IconLeaf = ValidHexColor(v, c.IconLeaf)
	}
	if v, ok := s.getSetting("icon_shape"); ok && (v == "rounded" || v == "square" || v == "circle") {
		c.IconShape = v
	}
	return c
}

func (s *Store) SaveAppConfig(c AppConfig) error {
	if c.TranslateTarget == "" {
		c.TranslateTarget = "en"
	}
	if c.AIModel == "" {
		c.AIModel = defaultAIModel
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
	c.AISummaryLevel = ValidSummaryLevel(c.AISummaryLevel, "brief")
	c.Accent = ValidAccent(c.Accent, "indigo")
	c.IconBG = ValidIconBG(c.IconBG, "#18181b")
	c.IconAccent = ValidHexColor(c.IconAccent, "") // blank = inherit the accent
	c.IconLeaf = ValidHexColor(c.IconLeaf, "#4ade80")
	switch c.IconShape {
	case "rounded", "square", "circle":
	default:
		c.IconShape = "rounded"
	}
	kv := map[string]string{
		"translate_api_key": c.TranslateAPIKey,
		"translate_target":  c.TranslateTarget,
		"ai_openrouter_key": c.AIKey,
		"ai_model":          c.AIModel,
		// Overrides are stored as-is: blank means "inherit ai_model".
		"ai_compose_model":   c.AIComposeModel,
		"ai_options_model":   c.AIOptionsModel,
		"ai_refine_model":    c.AIRefineModel,
		"ai_summarize_model": c.AISummarizeModel,
		"ai_tone":            c.AITone,
		"ai_brevity":         c.AIBrevity,
		"ai_reply_options":   strconv.Itoa(c.AIReplyOptions),
		"ai_language":        c.AILanguage,
		"ai_summary_level":   c.AISummaryLevel,
		"ui_accent":          c.Accent,
		"icon_bg":            c.IconBG,
		"icon_accent":        c.IconAccent,
		"icon_leaf":          c.IconLeaf,
		"icon_shape":         c.IconShape,
	}
	for k, v := range kv {
		if err := s.setSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}
