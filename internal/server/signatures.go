// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// sigVar is the per-identity signature payload embedded (as JSON) in the compose
// partial so the client can insert/replace the right variant on identity switch.
type sigVar struct {
	Plain    string `json:"plain"`
	HTML     string `json:"html"`
	Markdown string `json:"markdown"`
	Apply    string `json:"apply"` // always | first
}

// composeSignatures maps lowercased identity address -> its linked signature's
// variants. nil when nothing is linked. The HTML variant is sanitized here so
// the client can drop it into the contenteditable editor unescaped.
func (s *Server) composeSignatures() map[string]sigVar {
	links, err := s.store.IdentitySignatureMap()
	if err != nil || len(links) == 0 {
		return nil
	}
	out := map[string]sigVar{}
	for addr, id := range links {
		sig, err := s.store.SignatureByID(id)
		if err != nil || sig == nil {
			continue
		}
		out[addr] = sigVar{
			Plain:    sig.TextPlain,
			HTML:     mail.SanitizeComposeHTML(sig.HTML),
			Markdown: sig.Markdown,
			Apply:    sig.ApplyMode,
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// renderCompose fills in the per-identity signature map (unless already set) and
// renders the compose partial. Every compose render routes through here so the
// client always has the signature data for the From selector.
func (s *Server) renderCompose(w http.ResponseWriter, view composeView) {
	if view.Signatures == nil {
		view.Signatures = s.composeSignatures()
	}
	if view.Templates == nil {
		view.Templates, _ = s.store.ListTemplates()
	}
	// Normalize here so the format selector always has an option to mark
	// selected, whatever the pref/draft/posted value was.
	view.Mode = validMode(view.Mode)
	s.renderPartial(w, "compose", view)
}

// identityLink pairs a selectable send-as identity with its currently linked
// signature id (0 = none), for the Accounts settings picker.
type identityLink struct {
	Identity
	SignatureID int64
}

func (s *Server) identityLinks() []identityLink {
	m, _ := s.store.IdentitySignatureMap()
	var out []identityLink
	for _, id := range identities(s.accounts()) {
		out = append(out, identityLink{Identity: id, SignatureID: m[strings.ToLower(strings.TrimSpace(id.Address))]})
	}
	return out
}

func (s *Server) renderSignaturesManager(w http.ResponseWriter, r *http.Request) {
	sigs, err := s.store.ListSignatures()
	if err != nil {
		slog.Error("signatures: list", "err", err)
		http.Error(w, "Couldn't load signatures.", http.StatusInternalServerError)
		return
	}
	s.renderPartial(w, "signatures_manager", map[string]any{
		"CSRF":       auth.EnsureCSRF(w, r, s.secure),
		"Signatures": sigs,
		"Accounts":   s.accounts(),
		"Identities": s.identityLinks(),
	})
}

func (s *Server) handleSignaturesManager(w http.ResponseWriter, r *http.Request) {
	s.renderSignaturesManager(w, r)
}

// handleSignatureSave creates or updates a signature, then re-renders the
// manager. Part of the same one-big-form settings page, so it posts via htmx
// like accounts do (not a nested <form>).
func (s *Server) handleSignatureSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	sig := &store.Signature{
		ID:        id,
		Account:   strings.TrimSpace(r.PostFormValue("account")),
		Name:      strings.TrimSpace(r.PostFormValue("name")),
		TextPlain: r.PostFormValue("text_plain"),
		HTML:      r.PostFormValue("html"),
		Markdown:  r.PostFormValue("markdown"),
		ApplyMode: r.PostFormValue("apply_mode"),
	}
	if sig.Name == "" {
		http.Error(w, "Give the signature a name.", http.StatusBadRequest)
		return
	}
	if err := s.store.UpsertSignature(sig); err != nil {
		slog.Error("signature save", "err", err)
		http.Error(w, "Couldn't save the signature.", http.StatusInternalServerError)
		return
	}
	s.mail.Toast("Signature saved: " + sig.Name)
	s.renderSignaturesManager(w, r)
}

func (s *Server) handleSignatureDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteSignature(id); err != nil {
		slog.Error("signature delete", "err", err)
		http.Error(w, "Couldn't remove the signature.", http.StatusInternalServerError)
		return
	}
	s.mail.Toast("Signature removed")
	s.renderSignaturesManager(w, r)
}

// handleSignatureLink sets (or clears, signature_id 0) the signature linked to a
// sender identity. Fired by the per-identity picker's onchange.
func (s *Server) handleSignatureLink(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// The picker is named per identity ("signature_id:<address>") so the rows
	// can't shadow each other; the row that fired says which one it is.
	addr := r.PostFormValue("address")
	sigID, _ := strconv.ParseInt(r.PostFormValue("signature_id:"+addr), 10, 64)
	if err := s.store.SetIdentitySignature(addr, sigID); err != nil {
		slog.Error("signature link", "err", err)
		http.Error(w, "Couldn't link the signature.", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
