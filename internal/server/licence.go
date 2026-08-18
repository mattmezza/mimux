// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/store"
)

// The licence key box in Settings → API. This side only stores the key and
// shows what the pro layer made of it: enforcement lives in pro/ (ELv2) and a
// free build has none, which is why the status line is read out of the database
// rather than computed here.

// licenceView is the licence block's data.
type licenceView struct {
	Key     string // masked; blank when nothing is stored
	Status  string // the pro layer's last evaluation; blank in a free build
	FromEnv bool   // MIMUX_LICENCE_KEY is set, and it wins over the stored key
}

func (s *Server) licenceView() licenceView {
	key, _ := s.store.Setting(store.SettingLicenceKey)
	status, _ := s.store.Setting(store.SettingLicenceStatus)
	return licenceView{Key: maskLicenceKey(key), Status: status, FromEnv: s.cfg.LicenceKey != ""}
}

// maskLicenceKey keeps the parts that identify a key at a glance and drops the
// rest, so a settings screenshot does not hand the key over.
func maskLicenceKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 24 {
		return key[:4] + "…"
	}
	return key[:18] + "…" + key[len(key)-6:]
}

func (s *Server) renderLicence(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "licence", map[string]any{
		"CSRF":    auth.EnsureCSRF(w, r, s.secure),
		"Licence": s.licenceView(),
	})
}

func (s *Server) handleLicenceManager(w http.ResponseWriter, r *http.Request) {
	s.renderLicence(w, r)
}

// handleLicenceSave stores a pasted key. It is not validated here — only pro/
// holds the public key, and a free build rejecting a perfectly good key would
// be worse than storing one that a pro build will then explain.
func (s *Server) handleLicenceSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.PostFormValue("licence_key"))
	if key == "" {
		http.Error(w, "Paste the licence key first.", http.StatusBadRequest)
		return
	}
	if err := s.store.SetSetting(store.SettingLicenceKey, key); err != nil {
		slog.Error("licence: save", "err", err) // never the key
		http.Error(w, "Couldn't save the licence key.", http.StatusInternalServerError)
		return
	}
	// The stored status describes the old key until the pro layer re-evaluates
	// (within a minute); saying so beats showing a stale verdict as current.
	if err := s.store.SetSetting(store.SettingLicenceStatus, ""); err != nil {
		slog.Warn("licence: clearing status", "err", err)
	}
	s.mail.Toast("Licence key saved")
	s.renderLicence(w, r)
}

func (s *Server) handleLicenceRemove(w http.ResponseWriter, r *http.Request) {
	if err := s.store.SetSetting(store.SettingLicenceKey, ""); err != nil {
		slog.Error("licence: remove", "err", err)
		http.Error(w, "Couldn't remove the licence key.", http.StatusInternalServerError)
		return
	}
	if err := s.store.SetSetting(store.SettingLicenceStatus, ""); err != nil {
		slog.Warn("licence: clearing status", "err", err)
	}
	s.mail.Toast("Licence key removed")
	s.renderLicence(w, r)
}
