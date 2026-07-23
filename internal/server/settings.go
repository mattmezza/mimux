package server

import (
	"net/http"
	"strconv"

	"github.com/mattmezza/sm/internal/account"
	"github.com/mattmezza/sm/internal/auth"
	"github.com/mattmezza/sm/internal/store"
)

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	prefs := s.store.GetPrefs()
	s.render(w, "settings", map[string]any{
		"CSRF":         auth.EnsureCSRF(w, r, s.secure),
		"Prefs":        prefs,
		"Accounts":     s.accounts(),
		"AccountViews": s.accountViews(),
		"AppConfig":    s.store.GetAppConfig(),
		"Presets":      account.PresetNames(),
		"QAEditor":     qaEditorRows(prefs.QuickActions),
	})
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
	if err := s.store.SaveAppConfig(store.AppConfig{
		TranslateAPIKey: r.PostFormValue("translate_api_key"),
		TranslateTarget: r.PostFormValue("translate_target"),
		AIKey:           r.PostFormValue("ai_openrouter_key"),
		AIModel:         r.PostFormValue("ai_model"),
	}); err != nil {
		http.Error(w, "Couldn't save settings — please try again.", http.StatusInternalServerError)
		return
	}
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
