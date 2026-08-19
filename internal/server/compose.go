// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// Identity is one selectable "send as" address for the compose From menu:
// an account's primary address or one of its aliases.
type Identity struct {
	Name    string
	Address string
	IsAlias bool
}

// identities flattens accounts into their primary address plus any aliases.
// Template helper for the compose From selector.
func identities(accounts []config.Account) []Identity {
	var out []Identity
	for _, a := range accounts {
		name := a.SenderName
		if name == "" {
			name = a.Name
		}
		out = append(out, Identity{Name: name, Address: a.Email})
		for _, al := range a.Aliases {
			out = append(out, Identity{Name: al.Name, Address: al.Email, IsAlias: true})
		}
	}
	return out
}

func htmlEsc(s string) string { return html.EscapeString(s) }

// bareEmail moved down to internal/config (BareEmail) with accountForAddress.
func bareEmail(a string) string { return config.BareEmail(a) }

// accountForAddress moved down to internal/config (AccountForAddress) so the
// pro API's send endpoint resolves identities the same way compose does.
func (s *Server) accountForAddress(addr string) (config.Account, bool) {
	return config.AccountForAddress(s.cfg.Accounts, addr)
}

// composeView is the compose partial's template data.
type composeView struct {
	CSRF          string
	DraftID       int64
	Accounts      []config.Account
	Account       string
	From          string // selected send-as address (primary or alias)
	To, Cc, Bcc   string
	Subject       string
	Body          string
	Mode          string // plain|html|markdown compose editor
	Kind          string // new|reply|reply_all|forward
	Layout        string // fullscreen|popup|modal window layout
	InReplyTo     string // original Message-ID, for the In-Reply-To header
	References    string // full References header value to reuse
	ThreadContext string // for the embedded ai_reply partial
	UndoSendDelay int    // seconds the split-button "Send" waits (undo window)
	Autosave      bool   // opt-in: client debounce-saves the draft while typing
	Error         string
	// Signatures maps lowercased identity address -> its linked signature
	// variants, embedded so the client inserts the right one per From identity.
	Signatures map[string]sigVar
	// AutoSignature is true only for a fresh open (new/reply/forward) where the
	// client should auto-insert the linked signature. False for reopened drafts,
	// undo-send restores and send-error re-renders (body already carries it).
	AutoSignature bool
	// Templates are the saved message templates, alphabetical, offered in the
	// compose picker. Filled in by renderCompose.
	Templates []store.Template
	// Attachments are the files already kept with the saved draft (as opposed to
	// the ones sitting unsaved in the file input, which the client owns). Empty
	// until the draft has been saved at least once.
	Attachments []store.DraftAttachment
}

// validLayout whitelists a compose layout to the three the partial can render,
// so neither a stale pref nor a hand-posted form can inject anything else.
func validLayout(v, def string) string {
	switch v {
	case "fullscreen", "popup", "modal":
		return v
	}
	return def
}

// validMode whitelists a compose editor format to the three the partial can
// render, so a stale pref or a hand-posted form can't inject anything else.
func validMode(v string) string {
	switch v {
	case "plain", "html", "markdown":
		return v
	}
	return "plain"
}

// layoutForKind picks the layout pref that applies to a compose kind.
func layoutForKind(p store.Prefs, kind string) string {
	if kind == "" || kind == "new" {
		return validLayout(p.ComposeLayout, "fullscreen")
	}
	return validLayout(p.ReplyLayout, "popup")
}

// handleComposeNew serves GET /compose (blank) and GET
// /compose?reply=<id>&mode=reply|all|forward (prefilled). No draft row is
// created here: the compose opens with DraftID 0 and the row is created lazily
// by the first explicit "Save draft" (handleComposeDraftSave), so merely
// opening and closing compose leaves nothing behind.
func (s *Server) handleComposeNew(w http.ResponseWriter, r *http.Request) {
	prefs := s.store.GetPrefs()
	view := composeView{
		CSRF:          auth.EnsureCSRF(w, r, s.secure),
		Accounts:      s.cfg.Accounts,
		Kind:          "new",
		Mode:          prefs.ComposeMode,
		UndoSendDelay: prefs.UndoSendDelay,
		Autosave:      prefs.ComposeAutosave,
	}
	if len(s.cfg.Accounts) > 0 {
		view.Account = s.cfg.Accounts[0].Name
		view.From = s.cfg.Accounts[0].Email
	}
	// Reopening a saved local draft (from /drafts) just loads its fields —
	// no new row, no reply prefill.
	if draftID, err := strconv.ParseInt(r.URL.Query().Get("draft"), 10, 64); err == nil && draftID > 0 {
		if d, err := s.store.DraftByID(draftID); err == nil && d != nil {
			s.renderDraft(w, view, prefs, d)
			return
		}
	}
	// Editing a draft written in another client: adopt it first (see
	// mail.Manager.AdoptDraft), then it is a local draft like any other.
	if msgID, err := strconv.ParseInt(r.URL.Query().Get("adopt"), 10, 64); err == nil && msgID > 0 {
		s.adoptAndOpen(w, r, view, prefs, msgID)
		return
	}
	if replyID, err := strconv.ParseInt(r.URL.Query().Get("reply"), 10, 64); err == nil && replyID > 0 {
		if orig, err := s.store.MessageByID(replyID); err == nil && orig != nil {
			s.prefillReply(&view, orig, r.URL.Query().Get("mode"))
		}
	}
	// Layout depends on the final Kind, so resolve it after the reply prefill.
	view.Layout = layoutForKind(prefs, view.Kind)
	// Fresh open (new/reply/forward): the client auto-inserts the linked signature.
	view.AutoSignature = true
	s.renderCompose(w, view)
}

// renderDraft opens a stored draft in compose: its fields, its kept files, and
// the layout its kind asks for. view supplies what the request knows (CSRF,
// accounts, prefs) and the draft supplies the rest.
func (s *Server) renderDraft(w http.ResponseWriter, view composeView, prefs store.Prefs, d *store.Draft) {
	refs := ""
	if d.InReplyTo != "" {
		// NOTE: a reopened draft only remembers its immediate parent, not the
		// full References chain — still threads correctly via In-Reply-To in
		// every mail client that matters.
		refs = "<" + d.InReplyTo + ">"
	}
	from := ""
	if ac, ok := s.accountByName(d.Account); ok {
		from = ac.Email
	}
	mode := d.Mode
	if mode == "" {
		mode = view.Mode
	}
	atts, err := s.store.DraftAttachments(d.ID)
	if err != nil {
		slog.Error("compose: draft attachments", "draft", d.ID, "err", err)
	}
	s.renderCompose(w, composeView{
		CSRF: view.CSRF, Accounts: view.Accounts, DraftID: d.ID, Account: d.Account, From: from,
		To: d.To, Cc: d.Cc, Bcc: d.Bcc, Subject: d.Subject, Body: d.Body, Mode: mode,
		Kind: d.Kind, InReplyTo: d.InReplyTo, References: refs, UndoSendDelay: view.UndoSendDelay,
		Layout: layoutForKind(prefs, d.Kind), Autosave: view.Autosave, Attachments: atts,
	})
}

// adoptAndOpen opens a draft that lives only in the mailbox — written on the
// phone, or in whatever client was to hand — by adopting it into a local draft
// and rendering that. A draft mimux cannot reproduce faithfully (encrypted,
// signed, inline images) is not adopted at all: the window stays shut, a toast
// says why, and the row's own link still opens it read-only, which is the same
// place this landed before drafts from elsewhere were editable.
func (s *Server) adoptAndOpen(w http.ResponseWriter, r *http.Request, view composeView, prefs store.Prefs, msgID int64) {
	msg, err := s.store.MessageByID(msgID)
	if err != nil || msg == nil {
		http.NotFound(w, r)
		return
	}
	d, err := s.mail.AdoptDraft(r.Context(), msg)
	if err != nil {
		slog.Warn("drafts: not adoptable", "message", msgID, "err", err)
		s.mail.Toast("This draft can't be edited here — open it to read it.")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.renderDraft(w, view, prefs, d)
}

// prefillReply fills the To/Cc/Subject/Body/threading fields of view for a
// reply, reply-all or forward of orig.
func (s *Server) prefillReply(view *composeView, orig *store.Message, mode string) {
	view.Account = orig.Account
	self := orig.Account
	if ac, ok := s.accountByName(orig.Account); ok {
		self = ac.Email
		// Reply from the alias the mail was delivered to, when it was one.
		view.From = ac.Email
		if via := receivedAlias(ac.Name, orig.ToAddresses, orig.CcAddresses); via != "" {
			view.From = via
		}
	}
	from := orig.FromAddress
	if orig.FromName != "" {
		from = orig.FromName + " <" + orig.FromAddress + ">"
	}
	switch mode {
	case "all":
		view.Kind = "reply_all"
		to, cc := mail.ReplyAllRecipients(self,
			mail.SplitAddrList(orig.FromAddress), mail.SplitAddrList(orig.ToAddresses), mail.SplitAddrList(orig.CcAddresses))
		view.To, view.Cc = joinAddrList(to), joinAddrList(cc)
	case "forward":
		view.Kind = "forward"
	default:
		view.Kind = "reply"
		view.To = joinAddrList(mail.ReplyRecipients(self, orig.FromAddress))
	}
	view.Subject = mail.PrefixSubject(view.Kind, orig.Subject)
	// NOTE: quotes the stored snippet (first ~2KB), not the full fetched
	// body — reusing mail.Body would need an HTML->text conversion this
	// phase doesn't have. Good enough for a reply quote; revisit if users
	// complain about truncation.
	if view.Kind == "forward" {
		if view.Mode == "html" {
			view.Body = "<p><br></p><blockquote>---------- Forwarded message ----------<br>From: " +
				htmlEsc(from) + "<br>Date: " + orig.Date.Format("Mon, 2 Jan 2006 15:04") +
				"<br>Subject: " + htmlEsc(orig.Subject) + "<br><br>" + htmlEsc(orig.Snippet) + "</blockquote>"
		} else {
			view.Body = "\n\n---------- Forwarded message ----------\nFrom: " + from +
				"\nDate: " + orig.Date.Format("Mon, 2 Jan 2006 15:04") +
				"\nSubject: " + orig.Subject + "\n\n" + orig.Snippet
		}
	} else {
		if view.Mode == "html" {
			view.Body = mail.QuoteBodyHTML(orig.Date, from, orig.Snippet)
		} else {
			view.Body = "\n\n" + mail.QuoteBody(orig.Date, from, orig.Snippet)
		}
		view.InReplyTo = orig.MessageID
		view.References = mail.ComputeReferences(orig.Refs, orig.MessageID)
	}
	view.ThreadContext = from + " wrote:\n" + orig.Snippet
}

func joinAddrList(addrs []string) string {
	out := ""
	for i, a := range addrs {
		if i > 0 {
			out += ", "
		}
		out += a
	}
	return out
}

func (s *Server) accountByName(name string) (config.Account, bool) {
	for _, ac := range s.cfg.Accounts {
		if ac.Name == name {
			return ac, true
		}
	}
	return config.Account{}, false
}

// handleComposeDraftSave is the explicit draft-save endpoint: POST
// /compose/draft, fired only by the "Save draft" button (there is no autosave).
// The draft row is created lazily here on the first save (draft_id 0); the new
// id is handed back as an out-of-band swap so subsequent saves update the same
// row. Returns 204 once the id is known (the client leaves the modal open).
// Idempotent per revision: the local row is updated in place and publishDraft
// replaces whatever copy the last save left in the IMAP Drafts folder, so
// hitting save ten times leaves one draft, here and on the server.
func (s *Server) handleComposeDraftSave(w http.ResponseWriter, r *http.Request) {
	if err := parseComposeForm(w, r); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	id, _ := strconv.ParseInt(r.PostFormValue("draft_id"), 10, 64)
	// The From menu posts an address; store which account owns it.
	account := r.PostFormValue("account")
	if account == "" {
		if ac, ok := s.accountForAddress(r.PostFormValue("from")); ok {
			account = ac.Name
		}
	}
	d := &store.Draft{
		ID: id, Account: account,
		To: r.PostFormValue("to"), Cc: r.PostFormValue("cc"), Bcc: r.PostFormValue("bcc"),
		Subject: r.PostFormValue("subject"), Body: r.PostFormValue("body"),
		InReplyTo: r.PostFormValue("in_reply_to"), Kind: r.PostFormValue("kind"),
		Mode: r.PostFormValue("mode"),
	}
	if err := s.store.UpsertDraft(d); err != nil {
		slog.Error("compose: draft save", "err", err)
	}
	// The files picked since the last save join the draft before it is
	// published, so what lands in the Drafts folder is the message as the window
	// shows it. The client empties its own pending list on the way back, which
	// is why a save never stores the same file twice.
	atts, attErr := readAttachments(r)
	if attErr == "" {
		attErr = s.keepDraftAttachments(d.ID, atts)
	}
	if attErr != "" {
		s.mail.Toast(attErr)
	}
	s.publishDraft(d)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if id == 0 && d.ID != 0 {
		// First save: tell the open form its new draft id via OOB swap so later
		// saves target this row instead of creating more.
		_, _ = fmt.Fprintf(w, `<input type="hidden" id="compose-draft-id" name="draft_id" value="%d" hx-swap-oob="true">`, d.ID)
	}
	s.renderDraftAttachments(w, d.ID)
}

// keepDraftAttachments stores the freshly uploaded files against a draft,
// enforcing the send path's total cap across what is already kept plus what has
// just arrived. Returns a user-facing message when something was refused ("" if
// all of it was stored) — the save itself still stands, files and all the rest
// of the draft are two separate promises.
func (s *Server) keepDraftAttachments(draftID int64, atts []mail.OutAttachment) string {
	if draftID == 0 || len(atts) == 0 {
		return ""
	}
	total, err := s.store.DraftAttachmentsSize(draftID)
	if err != nil {
		slog.Error("compose: draft attachment size", "draft", draftID, "err", err)
		return ""
	}
	for _, a := range atts {
		total += int64(len(a.Data))
		if total > maxAttachTotal {
			return fmt.Sprintf("Attachments exceed the %dMB limit — %q was not kept with the draft.",
				maxAttachTotal>>20, a.Filename)
		}
		if err := s.store.AddDraftAttachment(draftID, &store.DraftAttachment{
			Filename: a.Filename, ContentType: a.ContentType, Data: a.Data,
		}); err != nil {
			slog.Error("compose: draft attachment save", "draft", draftID, "err", err)
			return fmt.Sprintf("Could not keep %q with the draft.", a.Filename)
		}
	}
	return ""
}

// renderDraftAttachments writes the draft's file chips as an out-of-band swap,
// so every save and every removal leaves the open compose window showing
// exactly what the draft holds.
func (s *Server) renderDraftAttachments(w http.ResponseWriter, draftID int64) {
	atts, err := s.store.DraftAttachments(draftID)
	if err != nil {
		slog.Error("compose: draft attachments", "draft", draftID, "err", err)
	}
	s.renderPartial(w, "compose_attachments", map[string]any{
		"DraftID": draftID, "Attachments": atts, "OOB": true,
	})
}

// handleDraftAttachmentDelete removes one stored file from a saved draft and
// republishes the draft without it.
func (s *Server) handleDraftAttachmentDelete(w http.ResponseWriter, r *http.Request) {
	draftID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	attID, aerr := strconv.ParseInt(chi.URLParam(r, "aid"), 10, 64)
	if err != nil || aerr != nil {
		http.NotFound(w, r)
		return
	}
	d, err := s.store.DraftByID(draftID)
	if err != nil || d == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteDraftAttachment(draftID, attID); err != nil {
		slog.Error("compose: draft attachment delete", "draft", draftID, "err", err)
	}
	// Re-save so the published copy loses the file too: the content changed, so
	// the mailbox revision is stale until the push replaces it.
	if err := s.store.UpsertDraft(d); err != nil {
		slog.Error("compose: draft re-save", "draft", draftID, "err", err)
	}
	s.publishDraft(d)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.renderDraftAttachments(w, draftID)
}

// parseComposeForm parses a compose POST in either shape it arrives in: a
// multipart body when files ride along (send, and a save that carries newly
// picked ones) or a plain form otherwise. The body is capped so a giant upload
// cannot exhaust disk; the per-file sum in readAttachments gives the clean
// 25MB error before this trips.
func parseComposeForm(w http.ResponseWriter, r *http.Request) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseForm()
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachTotal+(1<<20))
	// #nosec G120 -- body is capped by MaxBytesReader above
	return r.ParseMultipartForm(maxAttachTotal + (1 << 20))
}

// publishDraft appends the just-saved draft to the account's IMAP Drafts folder
// so it turns up on the user's phone. Background and silent on purpose: the
// SQLite row is already written and marked as owing a push, so an unreachable
// server costs a retry on the next sync cycle and never the draft. The user
// asked to save a draft; it is saved.
func (s *Server) publishDraft(d *store.Draft) {
	if d.ID == 0 || d.Account == "" {
		return
	}
	s.background(func(ctx context.Context) error {
		if err := s.mail.PushDraft(ctx, d); err != nil {
			slog.Warn("compose: draft push", "draft", d.ID, "err", err)
		}
		return nil
	})
}

// dropDraft deletes a draft — sent, or discarded — and clears the copy it
// published to the Drafts folder, so the other clients lose it too. Reading the
// row first is what makes the second half possible: it carries the folder and
// UID of the published revision.
func (s *Server) dropDraft(id int64) {
	if id <= 0 {
		return
	}
	d, err := s.store.DraftByID(id)
	if err != nil || d == nil {
		return
	}
	if err := s.store.DeleteDraft(id); err != nil {
		slog.Error("drafts: delete", "err", err)
	}
	s.background(func(ctx context.Context) error {
		if err := s.mail.DropDraft(ctx, d); err != nil {
			slog.Warn("drafts: mailbox copy not removed", "draft", id, "err", err)
		}
		return nil
	})
}

// handleComposePreview serves POST /compose/preview in two shapes:
//
//   - plain: render markdown source (POST body field) to the same sanitized
//     HTML used at send time, for the markdown editor's Preview tab.
//   - ?convert=1: the per-message format switcher. Converts the posted body
//     from mode_from to the newly picked mode and re-renders just the editor
//     block (#compose-body-wrap), so the attachment selection and every other
//     field survive the switch.
//
// NOTE: the switcher rides this route instead of getting its own
// /compose/mode, because the route table in server.go is owned by another
// in-flight change. Give it a route of its own once that lands.
func (s *Server) handleComposePreview(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("convert") != "" {
		mode := validMode(r.PostFormValue("mode"))
		s.renderPartial(w, "compose_body", composeView{
			Mode: mode,
			Body: mail.ConvertBody(r.PostFormValue("body"), validMode(r.PostFormValue("mode_from")), mode),
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// #nosec G705 -- RenderMarkdown escapes raw HTML (goldmark default) and runs
	// the bluemonday emailPolicy on its output; the fragment is sanitized.
	_, _ = io.WriteString(w, mail.RenderMarkdown(r.PostFormValue("body")))
}

// addrSuggestLimit is how many typeahead rows the dropdown shows.
const addrSuggestLimit = 6

// composeFragment isolates the address the user is still typing: the last
// comma-separated token of the field, minus a "Name <" prefix if they are
// mid-way through the angle-bracket form. Earlier, complete addresses in the
// field are not part of the query.
func composeFragment(v string) string {
	if i := strings.LastIndex(v, ","); i >= 0 {
		v = v[i+1:]
	}
	if i := strings.LastIndex(v, "<"); i >= 0 {
		v = v[i+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), ">"))
}

// handleAddressSuggest is the compose typeahead, mirroring /search/suggest: the
// field posts its own raw value (to/cc/bcc) and gets a dropdown fragment back.
func (s *Server) handleAddressSuggest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var raw string
	for _, f := range []string{"to", "cc", "bcc"} {
		if q.Has(f) {
			raw = q.Get(f)
			break
		}
	}
	frag := composeFragment(raw)
	var own []string
	for _, id := range identities(s.cfg.Accounts) {
		own = append(own, id.Address)
	}
	sug, err := s.store.SuggestAddresses(frag, own, addrSuggestLimit)
	if err != nil {
		slog.Error("address suggest", "err", err)
	}
	s.renderPartial(w, "address_suggest", map[string]any{"Suggestions": sug})
}

// maxAttachTotal caps the combined size of all attachments on one message —
// uploads, what a draft may keep, and what is read back off a foreign draft all
// answer to the same number.
const maxAttachTotal = mail.MaxAttachTotal

// readAttachments pulls the uploaded "attachments" files off a parsed multipart
// form, enforcing the total size cap. The second return is a user-facing error
// message ("" on success). Returns nil for a non-multipart request (no files).
// These are the files picked since the last save; whatever the draft already
// keeps is read from the store instead (see keepDraftAttachments).
func readAttachments(r *http.Request) ([]mail.OutAttachment, string) {
	if r.MultipartForm == nil {
		return nil, ""
	}
	var out []mail.OutAttachment
	var total int64
	for _, fh := range r.MultipartForm.File["attachments"] {
		if fh.Size == 0 {
			continue // empty file input slot
		}
		total += fh.Size
		if total > maxAttachTotal {
			return nil, fmt.Sprintf("Attachments exceed the %dMB limit.", maxAttachTotal>>20)
		}
		f, err := fh.Open()
		if err != nil {
			return nil, fmt.Sprintf("Could not read attachment %q.", fh.Filename)
		}
		data, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Sprintf("Could not read attachment %q.", fh.Filename)
		}
		out = append(out, mail.OutAttachment{
			Filename:    fh.Filename,
			ContentType: fh.Header.Get("Content-Type"),
			Data:        data,
		})
	}
	return out, ""
}

// handleComposeSend is POST /compose. On success it deletes the draft and
// returns 204 (the client closes the modal + shows a toast); on failure it
// re-renders the compose partial with an error banner, modal stays open,
// draft stays saved.
func (s *Server) handleComposeSend(w http.ResponseWriter, r *http.Request) {
	if err := parseComposeForm(w, r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	draftID, _ := strconv.ParseInt(r.PostFormValue("draft_id"), 10, 64)
	view := composeView{
		CSRF: auth.EnsureCSRF(w, r, s.secure), DraftID: draftID, Accounts: s.cfg.Accounts,
		From: r.PostFormValue("from"), To: r.PostFormValue("to"), Cc: r.PostFormValue("cc"), Bcc: r.PostFormValue("bcc"),
		Subject: r.PostFormValue("subject"), Body: r.PostFormValue("body"), Kind: r.PostFormValue("kind"),
		Mode:      r.PostFormValue("mode"),
		InReplyTo: r.PostFormValue("in_reply_to"), References: r.PostFormValue("references"),
	}
	// Keep the window in whatever layout it was opened in — and the draft's kept
	// files listed — across an error re-render.
	view.Attachments, _ = s.store.DraftAttachments(draftID)
	prefs := s.store.GetPrefs()
	view.Layout = validLayout(r.PostFormValue("layout"), layoutForKind(prefs, view.Kind))
	view.Autosave = prefs.ComposeAutosave
	if len(s.cfg.Accounts) == 0 {
		view.Error = "No accounts configured. Add one in Settings → Accounts."
		s.renderCompose(w, view)
		return
	}
	// Route SMTP by whichever account owns the chosen From address; the alias
	// (if any) rides along in ComposeInput.From. Fall back to the first account.
	if ac, ok := s.accountForAddress(view.From); ok {
		view.Account = ac.Name
	} else {
		view.Account = s.cfg.Accounts[0].Name
		if view.From == "" {
			view.From = s.cfg.Accounts[0].Email
		}
	}
	to := mail.SplitAddrList(view.To)
	if len(to) == 0 {
		view.Error = "Add at least one recipient."
		s.renderCompose(w, view)
		return
	}
	// The draft's kept files go out first, then whatever was picked since the
	// last save: the send attaches exactly what the compose window shows.
	atts, attErr := s.draftAttachments(draftID)
	if attErr == "" {
		var fresh []mail.OutAttachment
		fresh, attErr = readAttachments(r)
		atts = append(atts, fresh...)
	}
	if attErr != "" {
		view.Error = attErr
		s.renderCompose(w, view)
		return
	}
	in := mail.ComposeInput{
		To: to, Cc: mail.SplitAddrList(view.Cc), Bcc: mail.SplitAddrList(view.Bcc),
		Subject: view.Subject, Body: view.Body, Mode: view.Mode, From: view.From,
		InReplyTo: view.InReplyTo, References: view.References, Attachments: atts,
	}

	// "now" sends immediately (no undo window); "later" and "schedule" enqueue
	// on the outbox for the scheduler to deliver.
	if r.PostFormValue("send_mode") == "now" {
		if _, err := s.mail.Send(r.Context(), view.Account, in); err != nil {
			slog.Error("compose: send", "account", view.Account, "err", err)
			view.Error = "Could not send: " + err.Error()
			s.renderCompose(w, view)
			return
		}
		s.dropDraft(draftID)
		s.mail.Toast("Sent.")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	sendAt := time.Now().Add(time.Duration(s.store.GetPrefs().UndoSendDelay) * time.Second)
	scheduled := false
	if r.PostFormValue("send_mode") == "schedule" {
		t, err := time.Parse(time.RFC3339, r.PostFormValue("send_at"))
		if err != nil {
			view.Error = "Pick a valid date and time to schedule."
			s.renderCompose(w, view)
			return
		}
		sendAt = t
		scheduled = true
	}
	o := &store.Outbox{
		Account: view.Account, From: view.From, To: view.To, Cc: view.Cc, Bcc: view.Bcc,
		Subject: view.Subject, Body: view.Body, Mode: view.Mode,
		InReplyTo: view.InReplyTo, References: view.References,
		Attachments: outAttachments(atts), SendAt: sendAt.UTC(),
	}
	if err := s.store.EnqueueOutbox(o); err != nil {
		slog.Error("compose: enqueue", "err", err)
		view.Error = "Could not queue the message: " + err.Error()
		s.renderCompose(w, view)
		return
	}
	s.dropDraft(draftID)
	if scheduled {
		w.Header().Set("Mimux-Scheduled", sendAt.UTC().Format(time.RFC3339))
	} else {
		w.Header().Set("Mimux-Outbox-Id", strconv.FormatInt(o.ID, 10))
	}
	w.WriteHeader(http.StatusNoContent)
}

// draftAttachments loads a draft's kept files in the form the send path
// attaches. The second return is a user-facing error message ("" on success).
func (s *Server) draftAttachments(draftID int64) ([]mail.OutAttachment, string) {
	if draftID == 0 {
		return nil, ""
	}
	kept, err := s.store.DraftAttachments(draftID)
	if err != nil {
		slog.Error("compose: draft attachments", "draft", draftID, "err", err)
		return nil, "Could not read the files kept with this draft."
	}
	out := make([]mail.OutAttachment, len(kept))
	for i, a := range kept {
		out[i] = mail.OutAttachment{Filename: a.Filename, ContentType: a.ContentType, Data: a.Data}
	}
	return out, ""
}

// outAttachments converts freshly-uploaded attachments to the store's persisted
// form (so a queued message survives a restart).
func outAttachments(atts []mail.OutAttachment) []store.OutAttachment {
	out := make([]store.OutAttachment, len(atts))
	for i, a := range atts {
		out[i] = store.OutAttachment{Filename: a.Filename, ContentType: a.ContentType, Data: a.Data}
	}
	return out
}

// handleOutboxUndo cancels a just-queued delayed send and reopens the compose
// window with the draft restored, attachments included: they cannot go back
// into a file input, but they can go back into the draft, which is where the
// compose window now reads them from. Wired to the toast "Undo" action.
func (s *Server) handleOutboxUndo(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	o, err := s.store.OutboxByID(id)
	if err != nil || o == nil {
		http.NotFound(w, r)
		return
	}
	ok, err := s.store.CancelOutbox(id)
	if err != nil {
		http.Error(w, "undo failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		// Already sent/sending: the undo window closed.
		s.mail.Toast("Too late to undo — the message was already sent.")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Restore as a local draft so it can be reopened/saved, then reopen compose.
	d := &store.Draft{
		Account: o.Account, To: o.To, Cc: o.Cc, Bcc: o.Bcc, Subject: o.Subject,
		Body: o.Body, InReplyTo: o.InReplyTo, Kind: "new", Mode: o.Mode,
	}
	_ = s.store.UpsertDraft(d)
	for _, a := range o.Attachments {
		if err := s.store.AddDraftAttachment(d.ID, &store.DraftAttachment{
			Filename: a.Filename, ContentType: a.ContentType, Data: a.Data,
		}); err != nil {
			slog.Error("outbox: undo attachment", "outbox", id, "err", err)
		}
	}
	kept, _ := s.store.DraftAttachments(d.ID)
	prefs := s.store.GetPrefs()
	from := o.From
	if from == "" {
		if ac, ok := s.accountByName(o.Account); ok {
			from = ac.Email
		}
	}
	refs := o.References
	if refs == "" && o.InReplyTo != "" {
		refs = "<" + o.InReplyTo + ">"
	}
	s.renderCompose(w, composeView{
		CSRF: auth.EnsureCSRF(w, r, s.secure), Accounts: s.cfg.Accounts, DraftID: d.ID,
		Account: o.Account, From: from, To: o.To, Cc: o.Cc, Bcc: o.Bcc, Subject: o.Subject,
		Body: o.Body, Mode: o.Mode, Kind: "new", InReplyTo: o.InReplyTo, References: refs,
		UndoSendDelay: prefs.UndoSendDelay, Layout: layoutForKind(prefs, "new"), Autosave: prefs.ComposeAutosave,
		Attachments: kept,
	})
}

// --- the drafts page: every unfinished message, wherever it was written ---

// draftRow is one line of that page. A draft written here has an Edit id and
// opens in compose straight away; one written in another client has only a
// mailbox copy, and its Edit link adopts it (see adoptAndOpen) first.
type draftRow struct {
	Edit     int64 // local draft id, 0 for a draft that only exists on the server
	Message  int64 // store message id of the mailbox copy, 0 if not published yet
	FolderID int64
	Account  string
	Subject  string
	To       string
	Updated  time.Time
}

// draftCopyKey identifies one copy in a mailbox by where it sits, for the
// dedup above.
func draftCopyKey(folderID int64, uid uint32) string {
	return strconv.FormatInt(folderID, 10) + "\x00" + strconv.FormatUint(uint64(uid), 10)
}

// draftsPerFolder caps how much of a Drafts folder the page reads. Nobody has
// more unfinished mail than this, and the ones that matter are the recent ones.
const draftsPerFolder = 200

// draftRows merges the local drafts with what is sitting in each account's IMAP
// Drafts folder. A local draft and its own published copy are one message, not
// two, so the mailbox copy is dropped when its Message-ID matches a local row —
// which is exactly what the stable Message-ID on the draft is for.
func (s *Server) draftRows() ([]draftRow, error) {
	drafts, err := s.store.ListDrafts()
	if err != nil {
		return nil, err
	}
	rows := make([]draftRow, 0, len(drafts))
	mine := map[string]bool{}
	for _, d := range drafts {
		if d.MessageID != "" {
			mine[d.Account+"\x00"+d.MessageID] = true
		}
		// A draft adopted from another client may have arrived without a
		// Message-ID; where its copy sits identifies it until the first save
		// stamps one on.
		if d.UID != 0 {
			mine[draftCopyKey(d.FolderID, d.UID)] = true
		}
		rows = append(rows, draftRow{
			Edit: d.ID, Message: 0, FolderID: d.FolderID, Account: d.Account,
			Subject: d.Subject, To: d.To, Updated: d.UpdatedAt,
		})
	}
	for _, ac := range s.cfg.Accounts {
		f, err := s.store.FolderBySpecial(ac.Name, "drafts")
		if err != nil || f == nil {
			continue
		}
		msgs, err := s.store.ListMessages(f.ID, draftsPerFolder)
		if err != nil {
			slog.Error("drafts: folder list", "account", ac.Name, "err", err)
			continue
		}
		for _, m := range msgs {
			if m.MessageID != "" && mine[m.Account+"\x00"+m.MessageID] {
				continue
			}
			if mine[draftCopyKey(m.FolderID, m.UID)] {
				continue
			}
			rows = append(rows, draftRow{
				Message: m.ID, FolderID: m.FolderID, Account: m.Account,
				Subject: m.Subject, To: m.ToAddresses, Updated: m.Date,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Updated.After(rows[j].Updated) })
	return rows, nil
}

func (s *Server) handleDraftsPage(w http.ResponseWriter, r *http.Request) {
	drafts, err := s.draftRows()
	if err != nil {
		slog.Error("drafts: list", "err", err)
		http.Error(w, "failed to load drafts", http.StatusInternalServerError)
		return
	}
	scheduled, err := s.store.ListScheduled()
	if err != nil {
		slog.Error("drafts: scheduled", "err", err)
	}
	s.render(w, "drafts", map[string]any{
		"CSRF":      auth.EnsureCSRF(w, r, s.secure),
		"Sidebar":   s.sidebarData(),
		"Drafts":    drafts,
		"Scheduled": scheduled,
	})
}

func (s *Server) handleDraftDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.dropDraft(id)
	http.Redirect(w, r, "/drafts", http.StatusSeeOther)
}
