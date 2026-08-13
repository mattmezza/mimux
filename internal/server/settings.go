package server

import (
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/mattmezza/sm/internal/account"
	"github.com/mattmezza/sm/internal/auth"
	"github.com/mattmezza/sm/internal/store"
	"github.com/mattmezza/sm/internal/translate"
)

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	prefs := s.store.GetPrefs()
	sigs, _ := s.store.ListSignatures()
	tpls, _ := s.store.ListTemplates()
	msgCount, _ := s.store.MessageCount()
	s.render(w, "settings", map[string]any{
		"CSRF":         auth.EnsureCSRF(w, r, s.secure),
		"Prefs":        prefs,
		"Accounts":     s.accounts(),
		"AccountViews": s.accountViews(),
		"AppConfig":    s.store.GetAppConfig(),
		"Presets":      account.PresetNames(),
		"QAEditor":     qaEditorRows(prefs.QuickActions),
		"RowActions":   store.AllRowActions,
		"Signatures":   sigs,
		"Templates":    tpls,
		"Identities":   s.identityLinks(),
		"DBSize":       s.dbSizeHuman(),
		"MessageCount": msgCount,
	})
}

// dbSizeHuman totals the main SQLite file plus its -wal/-shm siblings (present
// under WAL journaling) and renders a humanized size for Settings → Syncing.
func (s *Server) dbSizeHuman() string {
	var total int64
	for _, suffix := range [...]string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(s.cfg.DB.Path + suffix); err == nil {
			total += fi.Size()
		}
	}
	return humanBytes(total)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	p := store.Prefs{
		MarkReadDelay:    atoiDefault(r.PostFormValue("mark_read_delay"), 0),
		SyncIntervalMin:  atoiDefault(r.PostFormValue("sync_interval_min"), 5),
		PreviewLines:     atoiDefault(r.PostFormValue("preview_lines"), 1),
		ShowAvatar:       r.PostFormValue("show_avatar") != "",
		ShowAccountBadge: r.PostFormValue("show_account_badge") != "",
		ShowAttachMarker: r.PostFormValue("show_attach_marker") != "",
		ShowFavicon:      r.PostFormValue("show_favicon") != "",
		HideAvatarMobile: r.PostFormValue("hide_avatar_mobile") != "",
		DarkMessages:     r.PostFormValue("dark_messages") != "",
		RememberMsgTheme: r.PostFormValue("remember_msg_theme") != "",
		SyncMonths:       atoiDefault(r.PostFormValue("sync_months"), 0),
		MaxPerSync:       atoiDefault(r.PostFormValue("max_per_sync"), 500),
		AccountColors:    map[string]string{},
		QuickActions:     store.JoinQuickActions(store.SplitQuickActions(r.PostFormValue("quick_actions"))),
		SearchScope:      r.PostFormValue("search_scope"),
		ComposeMode:      r.PostFormValue("compose_mode"),
		ComposeLayout:    r.PostFormValue("compose_layout"),
		ReplyLayout:      r.PostFormValue("reply_layout"),
		UndoSendDelay:    atoiDefault(r.PostFormValue("undo_send_delay"), 5),
		ThreadOrder:      r.PostFormValue("thread_order"),
		RowDoubleAction:  store.ValidRowAction(r.PostFormValue("row_double_action"), "unread"),
		SwipeLeftAction:  store.ValidRowAction(r.PostFormValue("swipe_left_action"), "none"),
		SwipeRightAction: store.ValidRowAction(r.PostFormValue("swipe_right_action"), "unread"),
	}
	switch p.UndoSendDelay {
	case 3, 5, 10:
	default:
		p.UndoSendDelay = 5
	}
	switch p.ThreadOrder {
	case "oldest", "newest":
	default:
		p.ThreadOrder = "oldest"
	}
	switch p.SearchScope {
	case "all", "account", "folder":
	default:
		p.SearchScope = "all"
	}
	switch p.ComposeMode {
	case "plain", "html", "markdown":
	default:
		p.ComposeMode = "html"
	}
	switch p.ComposeLayout {
	case "fullscreen", "popup", "modal":
	default:
		p.ComposeLayout = "fullscreen"
	}
	switch p.ReplyLayout {
	case "fullscreen", "popup", "modal":
	default:
		p.ReplyLayout = "popup"
	}
	if p.SyncMonths < 0 {
		p.SyncMonths = 0
	}
	if p.MarkReadDelay < 0 {
		p.MarkReadDelay = 0
	}
	if p.SyncIntervalMin < 1 {
		p.SyncIntervalMin = 1
	}
	if p.PreviewLines < 0 {
		p.PreviewLines = 0
	} else if p.PreviewLines > 3 {
		p.PreviewLines = 3
	}
	if p.MaxPerSync < 1 {
		p.MaxPerSync = 500
	}
	for _, a := range s.accounts() {
		if c := r.PostFormValue("color:" + a.Name); c != "" {
			p.AccountColors[a.Name] = c
		}
	}
	if err := s.store.SavePrefs(p); err != nil {
		http.Error(w, "Couldn't save settings — please try again.", http.StatusInternalServerError)
		return
	}
	// The form is a <select> of known languages; a posted code that isn't one
	// falls back to English rather than being stored and later sent to Google.
	target := r.PostFormValue("translate_target")
	if !translate.Supported(target) {
		target = "en"
	}
	if err := s.store.SaveAppConfig(store.AppConfig{
		TranslateAPIKey: r.PostFormValue("translate_api_key"),
		TranslateTarget: target,
		AIKey:           r.PostFormValue("ai_openrouter_key"),
		AIModel:         r.PostFormValue("ai_model"),
		AITone:          r.PostFormValue("ai_tone"),
		AIBrevity:       r.PostFormValue("ai_brevity"),
		AIReplyOptions:  atoiDefault(r.PostFormValue("ai_reply_options"), 3),
		AILanguage:      r.PostFormValue("ai_language"),
		AISummaryLevel:  r.PostFormValue("ai_summary_level"),
	}); err != nil {
		http.Error(w, "Couldn't save settings — please try again.", http.StatusInternalServerError)
		return
	}
	// Per-account sync overrides (Settings → Syncing, per-account section):
	// blank inherits the global values just saved above.
	for _, a := range s.accounts() {
		interval := parseOverride(r.PostFormValue("sync_interval_min:"+a.Name), 1)
		maxPerSync := parseOverride(r.PostFormValue("max_per_sync:"+a.Name), 1)
		syncMonths := parseOverride(r.PostFormValue("sync_months:"+a.Name), 0)
		if err := s.store.SetAccountSyncOverrides(a.Name, interval, maxPerSync, syncMonths); err != nil {
			http.Error(w, "Couldn't save settings — please try again.", http.StatusInternalServerError)
			return
		}
	}
	// Reload so running workers pick up any changed per-account override.
	s.mail.Reload()
	if r.Header.Get("HX-Request") != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// qaEditorRows builds the ordered rows for the settings quick-action editor:
// bar actions, then menu actions, then the hidden remainder. Maps (not a
// struct) so html/template's script-context escaping emits lowercase JSON keys
// for the Alpine component.
func qaEditorRows(pref string) []map[string]string {
	label := map[string]string{}
	for _, a := range store.AllQuickActions {
		label[a.ID] = a.Label
	}
	bar, menu := store.SplitQuickActions(pref)
	var rows []map[string]string
	placed := map[string]bool{}
	add := func(ids []string, place string) {
		for _, id := range ids {
			placed[id] = true
			rows = append(rows, map[string]string{"id": id, "label": label[id], "place": place})
		}
	}
	add(bar, "bar")
	add(menu, "menu")
	for _, a := range store.AllQuickActions {
		if !placed[a.ID] {
			rows = append(rows, map[string]string{"id": a.ID, "label": a.Label, "place": "hidden"})
		}
	}
	return rows
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// parseOverride parses a per-account sync-override field: blank (or
// unparseable) means "inherit the global value" (nil); otherwise it's clamped
// like its global counterpart.
func parseOverride(s string, min int) *int {
	if s == "" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	if n < min {
		n = min
	}
	return &n
}
