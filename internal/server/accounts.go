package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/sm/internal/account"
	"github.com/mattmezza/sm/internal/auth"
	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/store"
)

// accountFromForm builds an account from the add/edit form fields.
func accountFromForm(r *http.Request) config.Account {
	a := config.Account{
		Name:               strings.TrimSpace(r.PostFormValue("name")),
		Label:              strings.TrimSpace(r.PostFormValue("label")),
		SenderName:         strings.TrimSpace(r.PostFormValue("sender_name")),
		Provider:           strings.TrimSpace(r.PostFormValue("provider")),
		Email:              strings.TrimSpace(r.PostFormValue("email")),
		Auth:               r.PostFormValue("auth"),
		Password:           r.PostFormValue("password"),
		OAuth2ClientID:     strings.TrimSpace(r.PostFormValue("oauth2_client_id")),
		OAuth2ClientSecret: strings.TrimSpace(r.PostFormValue("oauth2_client_secret")),
		IMAPHost:           strings.TrimSpace(r.PostFormValue("imap_host")),
		IMAPPort:           atoiDefault(r.PostFormValue("imap_port"), 0),
		SMTPHost:           strings.TrimSpace(r.PostFormValue("smtp_host")),
		SMTPPort:           atoiDefault(r.PostFormValue("smtp_port"), 0),
	}
	names := r.PostForm["alias_name"]
	emails := r.PostForm["alias_email"]
	for i := range emails {
		email := strings.TrimSpace(emails[i])
		if email == "" {
			continue
		}
		name := ""
		if i < len(names) {
			name = strings.TrimSpace(names[i])
		}
		a.Aliases = append(a.Aliases, config.Alias{Name: name, Email: email})
	}
	return a
}

// handleAccountSave creates or updates an account, then reloads the mail workers
// and re-renders the account manager list.
func (s *Server) handleAccountSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	a := accountFromForm(r)
	// On edit the name is fixed (it keys the synced mail); a rename is a
	// remove+add, which the user does explicitly. Label is the renameable part.
	if a.Name == "" || a.Email == "" {
		s.accountError(w, r, "Name and email are required.")
		return
	}
	if a.Auth != "oauth2" {
		a.Auth = "password"
	}
	// Secrets are never sent back to the edit form (not embedded in the page), so
	// a blank secret on an existing account means "keep the current one".
	if existing, _ := s.store.GetAccount(a.Name); existing != nil {
		if a.Password == "" {
			a.Password = existing.Password
		}
		if a.OAuth2ClientSecret == "" {
			a.OAuth2ClientSecret = existing.OAuth2ClientSecret
		}
	}
	if err := config.NormalizeAccount(&a); err != nil {
		s.accountError(w, r, err.Error())
		return
	}
	if err := s.store.UpsertAccount(a); err != nil {
		slog.Error("account save", "err", err)
		s.accountError(w, r, "Couldn't save the account.")
		return
	}
	s.refreshAccounts()
	s.mail.Reload()
	s.mail.Toast("Account saved: " + a.DisplayLabel())
	s.renderAccountsManager(w, r)
}

// handleAccountDelete removes an account and all of its synced mail.
func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.store.DeleteAccount(name); err != nil {
		slog.Error("account delete", "err", err)
		http.Error(w, "Couldn't remove the account.", http.StatusInternalServerError)
		return
	}
	s.refreshAccounts()
	s.mail.Reload()
	s.mail.Toast("Account removed: " + name)
	s.renderAccountsManager(w, r)
}

func (s *Server) accountError(w http.ResponseWriter, r *http.Request, msg string) {
	// Plain text: the client toasts responseText (set as textContent), and this
	// avoids reflecting the user-supplied account name into an HTML response.
	http.Error(w, msg, http.StatusBadRequest)
}

// accountView pairs an account with its live sync status for the manager list.
type accountView struct {
	config.Account
	State     string
	Message   string
	NeedsAuth bool
	CanOAuth  bool
}

// acctEditable is the non-secret account data embedded (as JSON) in each Edit
// button so the form can be prefilled client-side without ever shipping the
// password or oauth secret to the page.
type acctEditable struct {
	Name           string         `json:"name"`
	Label          string         `json:"label"`
	SenderName     string         `json:"sender_name"`
	Provider       string         `json:"provider"`
	Email          string         `json:"email"`
	Auth           string         `json:"auth"`
	IMAPHost       string         `json:"imap_host"`
	IMAPPort       int            `json:"imap_port"`
	SMTPHost       string         `json:"smtp_host"`
	SMTPPort       int            `json:"smtp_port"`
	OAuth2ClientID string         `json:"oauth2_client_id"`
	Aliases        []config.Alias `json:"aliases"`
	HasPassword    bool           `json:"has_password"`
	HasSecret      bool           `json:"has_secret"`
}

func (v accountView) Editable() acctEditable {
	return acctEditable{
		Name: v.Name, Label: v.Label, SenderName: v.SenderName, Provider: v.Provider, Email: v.Email,
		Auth: v.Auth, IMAPHost: v.IMAPHost, IMAPPort: v.IMAPPort,
		SMTPHost: v.SMTPHost, SMTPPort: v.SMTPPort, OAuth2ClientID: v.OAuth2ClientID,
		Aliases:     v.Aliases,
		HasPassword: v.Password != "",
		HasSecret:   v.OAuth2ClientSecret != "",
	}
}

func (s *Server) accountViews() []accountView {
	statusByName := map[string]struct{ State, Message string }{}
	for _, st := range s.mail.Status() {
		statusByName[st.Account] = struct{ State, Message string }{st.State, st.Message}
	}
	var out []accountView
	for _, a := range s.accounts() {
		v := accountView{Account: a, CanOAuth: account.SupportsOAuth(a.Provider)}
		if st, ok := statusByName[a.Name]; ok {
			v.State, v.Message = st.State, st.Message
		}
		if a.Auth == "oauth2" {
			if tok, _ := s.store.GetToken(a.Name); tok == nil || (tok.Access == "" && tok.Refresh == "") {
				v.NeedsAuth = true
			}
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) renderAccountsManager(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "accounts_manager", map[string]any{
		"CSRF":     auth.EnsureCSRF(w, r, s.secure),
		"Accounts": s.accountViews(),
		"Presets":  account.PresetNames(),
	})
}

// handleAccountsManager renders the account manager partial (used after edits or
// to refresh the list).
func (s *Server) handleAccountsManager(w http.ResponseWriter, r *http.Request) {
	s.renderAccountsManager(w, r)
}

// --- portable export / import ---

func (s *Server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	exp, err := s.store.Export()
	if err != nil {
		http.Error(w, "export failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="sm-config.json"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(exp); err != nil {
		slog.Error("config export", "err", err)
	}
}

func (s *Server) handleConfigImport(w http.ResponseWriter, r *http.Request) {
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "No file uploaded.", http.StatusBadRequest)
		return
	}
	defer func() { _ = f.Close() }()
	var exp store.ConfigExport
	if err := json.NewDecoder(f).Decode(&exp); err != nil {
		http.Error(w, "Not a valid config file.", http.StatusBadRequest)
		return
	}
	sum, err := s.store.Import(exp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.refreshAccounts()
	s.mail.Reload()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Zeros are shown on purpose: "0 labelled message(s)" is how the user learns
	// labels found no synced mail to re-attach to yet (see store.Import).
	_, _ = fmt.Fprintf(w,
		"Imported %d account(s), %d setting(s), %d token(s), %d filter(s), %d signature(s), "+
			"%d template(s), %d saved search(es), %d trusted sender(s), %d labelled message(s).",
		sum.Accounts, sum.Settings, sum.Tokens, sum.Filters, sum.Signatures,
		sum.Templates, sum.Searches, sum.Senders, sum.Labels)
}
