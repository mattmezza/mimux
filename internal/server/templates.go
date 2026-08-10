package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/sm/internal/auth"
	"github.com/mattmezza/sm/internal/store"
)

func (s *Server) renderTemplatesManager(w http.ResponseWriter, r *http.Request) {
	tpls, err := s.store.ListTemplates()
	if err != nil {
		slog.Error("templates: list", "err", err)
		http.Error(w, "Couldn't load templates.", http.StatusInternalServerError)
		return
	}
	s.renderPartial(w, "templates_manager", map[string]any{
		"CSRF":      auth.EnsureCSRF(w, r, s.secure),
		"Templates": tpls,
	})
}

func (s *Server) handleTemplatesManager(w http.ResponseWriter, r *http.Request) {
	s.renderTemplatesManager(w, r)
}

// handleTemplateSave creates or updates a template, then re-renders the manager.
// Posts via htmx like signatures do (the settings page is one big form, so this
// can't be a nested <form>).
func (s *Server) handleTemplateSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	tpl := &store.Template{
		ID:   id,
		Name: strings.TrimSpace(r.PostFormValue("name")),
		Body: r.PostFormValue("body"),
	}
	if tpl.Name == "" {
		http.Error(w, "Give the template a name.", http.StatusBadRequest)
		return
	}
	if err := s.store.UpsertTemplate(tpl); err != nil {
		slog.Error("template save", "err", err)
		http.Error(w, "Couldn't save the template.", http.StatusInternalServerError)
		return
	}
	s.mail.Toast("Template saved: " + tpl.Name)
	s.renderTemplatesManager(w, r)
}

func (s *Server) handleTemplateDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteTemplate(id); err != nil {
		slog.Error("template delete", "err", err)
		http.Error(w, "Couldn't remove the template.", http.StatusInternalServerError)
		return
	}
	s.mail.Toast("Template removed")
	s.renderTemplatesManager(w, r)
}
