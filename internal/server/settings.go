package server

import (
	"net/http"
	"strconv"

	"github.com/mattmezza/sm/internal/auth"
	"github.com/mattmezza/sm/internal/store"
)

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings", map[string]any{
		"CSRF":     auth.EnsureCSRF(w, r, s.secure),
		"Prefs":    s.store.GetPrefs(),
		"Accounts": s.cfg.Accounts,
	})
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	p := store.Prefs{
		MarkReadDelay:   atoiDefault(r.PostFormValue("mark_read_delay"), 0),
		SyncIntervalMin: atoiDefault(r.PostFormValue("sync_interval_min"), 5),
		PreviewLines:    atoiDefault(r.PostFormValue("preview_lines"), 1),
		ShowAvatar:      r.PostFormValue("show_avatar") != "",
		SyncMonths:      atoiDefault(r.PostFormValue("sync_months"), 0),
		AccountColors:   map[string]string{},
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
	for _, a := range s.cfg.Accounts {
		if c := r.PostFormValue("color:" + a.Name); c != "" {
			p.AccountColors[a.Name] = c
		}
	}
	if err := s.store.SavePrefs(p); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
