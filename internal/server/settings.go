// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/account"
	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
	"github.com/mattmezza/mimux/internal/translate"
)

// Settings is one page per section, at /settings/<id>. The section lives in the
// URL rather than in a hash or localStorage, so a link to "the webhooks screen"
// is a link, the Back button works, and the browser doesn't paint the wrong
// panel for a frame on a cold load.
//
// Pro marks the sections that only do something in a pro build: they still
// render (the free build stores what you configure), with a banner saying so.
// Form marks the ones that carry fields the page's Save button writes — the
// rest are self-contained managers that post their own htmx requests, and a
// Save button there would save nothing.
type settingsPage struct {
	ID, Label string
	Pro       bool
	Form      bool
}

var settingsSections = []settingsPage{
	{ID: "appearance", Label: "Appearance", Form: true},
	{ID: "reading", Label: "Reading", Form: true},
	{ID: "composing", Label: "Composing", Form: true},
	{ID: "quick-actions", Label: "Quick actions", Form: true},
	{ID: "notifications", Label: "Notifications", Form: true},
	{ID: "syncing", Label: "Syncing", Form: true},
	{ID: "search", Label: "Search", Form: true},
	{ID: "accounts", Label: "Accounts", Form: true},
	{ID: "signatures", Label: "Signatures"},
	{ID: "templates", Label: "Templates"},
	{ID: "api", Label: "API", Pro: true},
	{ID: "webhooks", Label: "Webhooks", Pro: true, Form: true},
	{ID: "integrations", Label: "Integrations", Form: true},
	{ID: "backup", Label: "Backup & restore"},
	// Last on purpose: it is the section you visit once, not the one you live in.
	{ID: "licence", Label: "Licence", Pro: true},
}

// defaultSettingsSection is where a bare /settings lands.
const defaultSettingsSection = "appearance"

// settingsSection returns the section with this id, or nil.
func settingsSection(id string) *settingsPage {
	for i := range settingsSections {
		if settingsSections[i].ID == id {
			return &settingsSections[i]
		}
	}
	return nil
}

// handleSettings keeps /settings working as a bookmark: it lands on the first
// section rather than rendering a fourteenth copy of the page.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/settings/"+defaultSettingsSection, http.StatusSeeOther)
}

func (s *Server) handleSettingsSection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "section")
	sec := settingsSection(id)
	if sec == nil {
		http.NotFound(w, r)
		return
	}
	data := s.settingsData(w, r)
	data["Section"] = sec.ID
	data["Page"] = *sec
	s.render(w, "settings", data)
}

// settingsData is the settings page's model. Built whole rather than per
// section: it is a dozen small queries on a page nobody opens in a loop, and a
// per-section switch here is a per-section bug waiting to happen.
func (s *Server) settingsData(w http.ResponseWriter, r *http.Request) map[string]any {
	prefs := s.store.GetPrefs()
	sigs, _ := s.store.ListSignatures()
	tpls, _ := s.store.ListTemplates()
	msgCount, _ := s.store.MessageCount()
	// Registered push devices, and the public key a new one needs to subscribe.
	// VAPIDPublicKey generates the pair on first call — invisible, one-off, and
	// it never prompts the user for anything.
	pushDevices, _ := s.store.ListPushSubs()
	apiTokens, _ := s.store.ListAPITokens()
	webhooks, _ := s.webhookViews()
	// Every account's folders, for the "Folders synced continuously" checkbox
	// list. Empty until an account has connected once — folders are discovered by
	// the first IMAP LIST, not configured.
	accountFolders := map[string][]store.Folder{}
	for _, a := range s.accounts() {
		accountFolders[a.Name], _ = s.store.ListFolders(a.Name)
	}
	return map[string]any{
		"CSRF":         auth.EnsureCSRF(w, r, s.secure),
		"Sections":     settingsSections,
		"Pro":          s.proView(),
		"Prefs":        prefs,
		"Accounts":     s.accounts(),
		"AccountViews": s.accountViews(),
		"AppConfig":    s.store.GetAppConfig(),
		"Presets":      account.PresetNames(),
		"QAEditor":     qaEditorRows(prefs.QuickActions),
		"RowActions":   store.AllRowActions,
		"NotifyScopes": store.AllNotifyScopes,
		"ReplyQuotes":  store.AllReplyQuotes,
		"PushDevices":  pushDevices,
		"VAPIDKey":     s.mail.VAPIDPublicKey(),
		"Accents":      store.AllAccents,
		"IconShapes":   iconShapes,
		"AvatarShapes": avatarShapes,
		// Resolved mark colour, so the colour input always has a value even
		// when icon_accent is blank ("inherit the app accent").
		"IconMark":       iconMark(s.store.GetAppConfig()),
		"Signatures":     sigs,
		"Templates":      tpls,
		"APITokens":      apiTokens,
		"APIScopes":      store.APIScopes,
		"Webhooks":       webhooks,
		"WebhookEvents":  store.WebhookEvents,
		"Licence":        s.licenceView(),
		"Identities":     s.identityLinks(),
		"DBSize":         s.dbSizeHuman(),
		"MessageCount":   msgCount,
		"AccountFolders": accountFolders,
	}
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

// avatarShapes are the sender-avatar corner choices, for the Settings select.
var avatarShapes = []struct{ ID, Label string }{
	{"circle", "Circle"},
	{"rounded", "Rounded square"},
	{"square", "Square"},
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

// handleSettingsSave writes back the section the form came from.
//
// Each page posts a hidden `section`, and only that section's fields are read —
// the rest of Prefs and AppConfig is loaded from the database and written back
// untouched. Without that, saving the Reading page would post no ntfy URL, no
// AI key and no accent, and store the zero value of each.
//
// A post without a section applies every section, which is what the whole-page
// form used to do (and what the tests below still exercise).
func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	section := r.PostFormValue("section")
	if section != "" && settingsSection(section) == nil {
		http.Error(w, "unknown settings section", http.StatusBadRequest)
		return
	}
	in := func(id string) bool { return section == "" || section == id }

	p := s.store.GetPrefs()
	cfg := s.store.GetAppConfig()
	if in("appearance") {
		applyAppearance(&cfg, r)
	}
	if in("reading") {
		s.applyReading(&p, r)
	}
	if in("composing") {
		applyComposing(&p, r)
	}
	if in("quick-actions") {
		p.QuickActions = store.JoinQuickActions(store.SplitQuickActions(r.PostFormValue("quick_actions")))
	}
	if in("notifications") {
		applyNotifications(&p, r)
	}
	if in("syncing") {
		applySyncing(&p, r)
	}
	if in("search") {
		p.SearchScope = oneOf(r.PostFormValue("search_scope"), "all", "account", "folder")
	}
	if in("accounts") {
		for _, a := range s.accounts() {
			if c := r.PostFormValue("color:" + a.Name); c != "" {
				p.AccountColors[a.Name] = c
			}
		}
	}
	if in("webhooks") {
		// The endpoint list manages itself over htmx; this is the one field the
		// page's Save button writes.
		p.ExternalBurstLimit = atoiDefault(r.PostFormValue("external_burst_limit"), 200)
		if p.ExternalBurstLimit < 0 {
			p.ExternalBurstLimit = 0 // 0 = no cap
		}
	}
	if in("integrations") {
		applyIntegrations(&cfg, r)
	}
	if err := s.store.SavePrefs(p); err != nil {
		http.Error(w, "Couldn't save settings — please try again.", http.StatusInternalServerError)
		return
	}
	// SaveAppConfig validates every colour; nothing unvalidated reaches the icon
	// SVG or the accent <style>.
	if err := s.store.SaveAppConfig(cfg); err != nil {
		http.Error(w, "Couldn't save settings — please try again.", http.StatusInternalServerError)
		return
	}
	// Per-account sync overrides (Settings → Syncing, per-account section):
	// blank inherits the global values just saved above.
	if in("syncing") {
		for _, a := range s.accounts() {
			interval := parseOverride(r.PostFormValue("sync_interval_min:"+a.Name), 1)
			maxPerSync := parseOverride(r.PostFormValue("max_per_sync:"+a.Name), 1)
			syncMonths := parseOverride(r.PostFormValue("sync_months:"+a.Name), 0)
			bodyCache := parseOverride(r.PostFormValue("body_cache:"+a.Name), 0)
			if err := s.store.SetAccountSyncOverrides(a.Name, interval, maxPerSync, syncMonths, bodyCache); err != nil {
				http.Error(w, "Couldn't save settings — please try again.", http.StatusInternalServerError)
				return
			}
			// Which folders the steady loop keeps re-reading. A checkbox list, so
			// an absent field is a real "no" — same as every other checkbox on
			// this page. The inbox rides along as a hidden field (its box is
			// checked and disabled, and a disabled control is never submitted).
			if err := s.store.SetSyncedFolders(a.Name, postedInt64s(r, "sync_folder:"+a.Name)); err != nil {
				http.Error(w, "Couldn't save settings — please try again.", http.StatusInternalServerError)
				return
			}
		}
	}
	// Reload so running workers pick up any changed per-account override.
	s.mail.Reload()
	if r.Header.Get("HX-Request") != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Land back on the section the form came from, named by the registry rather
	// than by the post: the only strings that can reach the URL are the ids this
	// package declares.
	dest := defaultSettingsSection
	if p := settingsSection(section); p != nil {
		dest = p.ID
	}
	http.Redirect(w, r, "/settings/"+dest, http.StatusSeeOther)
}

// applyAppearance reads Settings → Appearance (the accent and the app icon).
func applyAppearance(c *store.AppConfig, r *http.Request) {
	c.Accent = r.PostFormValue("ui_accent")
	c.IconBG = r.PostFormValue("icon_bg")
	c.IconAccent = r.PostFormValue("icon_accent")
	c.IconLeaf = r.PostFormValue("icon_leaf")
	c.IconShape = r.PostFormValue("icon_shape")
}

// applyReading reads Settings → Reading.
func (s *Server) applyReading(p *store.Prefs, r *http.Request) {
	p.MarkReadDelay = atoiDefault(r.PostFormValue("mark_read_delay"), 0)
	if p.MarkReadDelay < 0 {
		p.MarkReadDelay = 0
	}
	p.PreviewDesktop = r.PostFormValue("preview_desktop") != ""
	p.PreviewDesktopLines = clampPreviewLines(atoiDefault(r.PostFormValue("preview_desktop_lines"), 1))
	p.PreviewMobile = r.PostFormValue("preview_mobile") != ""
	p.PreviewMobileLines = clampPreviewLines(atoiDefault(r.PostFormValue("preview_mobile_lines"), 1))
	p.ThreadOrder = oneOf(r.PostFormValue("thread_order"), "oldest", "newest")
	p.RowDoubleAction = store.ValidRowAction(r.PostFormValue("row_double_action"), "unread")
	p.SwipeLeftAction = store.ValidRowAction(r.PostFormValue("swipe_left_action"), "none")
	p.SwipeRightAction = store.ValidRowAction(r.PostFormValue("swipe_right_action"), "unread")
	p.ShowAccountBadge = r.PostFormValue("show_account_badge") != ""
	p.ShowAttachMarker = r.PostFormValue("show_attach_marker") != ""
	p.ShowListLabels = r.PostFormValue("show_list_labels") != ""
	p.DaySeparators = r.PostFormValue("day_separators") != ""
	p.DarkMessages = r.PostFormValue("dark_messages") != ""
	p.RememberMsgTheme = r.PostFormValue("remember_msg_theme") != ""
	// The avatar dependents (favicon/shape/hide-on-mobile) are disabled in the
	// UI when show_avatar is off, and a disabled control isn't submitted at
	// all — so an absent field here doesn't mean "off"/"circle", it means
	// "the browser didn't send it". Keep whatever was already stored instead
	// of overwriting it with the form's zero values, or turning avatars back
	// on later would come back to reset favicon/shape/mobile choices.
	if p.ShowAvatar = r.PostFormValue("show_avatar") != ""; p.ShowAvatar {
		p.ShowFavicon = r.PostFormValue("show_favicon") != ""
		p.HideAvatarMobile = r.PostFormValue("hide_avatar_mobile") != ""
		p.AvatarShape = oneOf(r.PostFormValue("avatar_shape"), "circle", "rounded", "square")
	}
}

// applyComposing reads Settings → Composing.
func applyComposing(p *store.Prefs, r *http.Request) {
	p.ComposeMode = oneOf(r.PostFormValue("compose_mode"), "html", "plain", "markdown")
	p.ComposeLayout = oneOf(r.PostFormValue("compose_layout"), "fullscreen", "popup", "modal")
	p.ReplyLayout = oneOf(r.PostFormValue("reply_layout"), "popup", "fullscreen", "modal")
	p.ComposeAutosave = r.PostFormValue("compose_autosave") != ""
	p.ReplyQuote = store.ValidReplyQuote(r.PostFormValue("reply_quote"), "all")
	// The line count is only read for the "lines" choice, but it is stored
	// whatever is picked: switching back to "first few lines" should find the
	// number you last typed, not the default.
	p.ReplyQuoteLines = atoiDefault(r.PostFormValue("reply_quote_lines"), store.DefaultReplyQuoteLines)
	if p.ReplyQuoteLines < 1 {
		p.ReplyQuoteLines = 1
	} else if p.ReplyQuoteLines > 200 {
		p.ReplyQuoteLines = 200
	}
	switch p.UndoSendDelay = atoiDefault(r.PostFormValue("undo_send_delay"), 5); p.UndoSendDelay {
	case 3, 5, 10:
	default:
		p.UndoSendDelay = 5
	}
}

// applyNotifications reads Settings → Notifications. Neither control is ever
// disabled in the UI — the whole section stays submittable whatever the master
// switch says — so unlike the avatar dependents there is nothing to preserve:
// an absent field really does mean the user cleared it.
func applyNotifications(p *store.Prefs, r *http.Request) {
	p.NotifyScope = store.ValidNotifyScope(r.PostFormValue("notify_scope"), "off")
	p.NtfyURL = strings.TrimSpace(r.PostFormValue("ntfy_url"))
	// A topic URL that isn't http(s) would be posted to on every new message and
	// fail every time; refuse it at the boundary instead.
	if u := p.NtfyURL; u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		p.NtfyURL = ""
	}
}

// applySyncing reads Settings → Syncing (the global half; the per-account
// overrides are written separately, outside the Prefs row).
func applySyncing(p *store.Prefs, r *http.Request) {
	p.SyncIntervalMin = atoiDefault(r.PostFormValue("sync_interval_min"), 5)
	if p.SyncIntervalMin < 1 {
		p.SyncIntervalMin = 1
	}
	p.MaxPerSync = atoiDefault(r.PostFormValue("max_per_sync"), 500)
	if p.MaxPerSync < 1 {
		p.MaxPerSync = 500
	}
	p.SyncMonths = atoiDefault(r.PostFormValue("sync_months"), 0)
	if p.SyncMonths < 0 {
		p.SyncMonths = 0
	}
	p.BodyCache = atoiDefault(r.PostFormValue("body_cache"), config.DefaultBodyCache)
	if p.BodyCache < 0 {
		p.BodyCache = 0 // 0 = off: no prefetching, no bodies kept on disk
	}
}

// applyIntegrations reads Settings → Integrations (Translate and AI).
func applyIntegrations(c *store.AppConfig, r *http.Request) {
	c.TranslateAPIKey = r.PostFormValue("translate_api_key")
	// The form is a <select> of known languages; a posted code that isn't one
	// falls back to English rather than being stored and later sent to Google.
	if c.TranslateTarget = r.PostFormValue("translate_target"); !translate.Supported(c.TranslateTarget) {
		c.TranslateTarget = "en"
	}
	c.AIKey = r.PostFormValue("ai_openrouter_key")
	c.AIModel = r.PostFormValue("ai_model")
	// Per-feature model overrides: blank inherits the default model above.
	c.AIComposeModel = strings.TrimSpace(r.PostFormValue("ai_compose_model"))
	c.AIOptionsModel = strings.TrimSpace(r.PostFormValue("ai_options_model"))
	c.AIRefineModel = strings.TrimSpace(r.PostFormValue("ai_refine_model"))
	c.AISummarizeModel = strings.TrimSpace(r.PostFormValue("ai_summarize_model"))
	c.AITone = r.PostFormValue("ai_tone")
	c.AIBrevity = r.PostFormValue("ai_brevity")
	c.AIReplyOptions = atoiDefault(r.PostFormValue("ai_reply_options"), 3)
	c.AILanguage = r.PostFormValue("ai_language")
	c.AISummaryLevel = r.PostFormValue("ai_summary_level")
}

// oneOf returns v when it is one of the allowed values, else the first one —
// which is the default by construction, so a hand-posted value cannot store
// something the UI would then fail to render.
func oneOf(v string, allowed ...string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return allowed[0]
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

// postedInt64s reads a repeated form field as ids, dropping anything that isn't
// one. r.ParseForm has already run.
func postedInt64s(r *http.Request, field string) []int64 {
	var out []int64
	for _, v := range r.PostForm[field] {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// clampPreviewLines bounds a preview line-count field to 1..5 — same rule for
// desktop and mobile, so one helper instead of two copies of an if/else.
func clampPreviewLines(n int) int {
	if n < 1 {
		return 1
	}
	if n > 5 {
		return 5
	}
	return n
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
