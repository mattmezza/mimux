package server

import (
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/sm/internal/auth"
	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/mail"
	"github.com/mattmezza/sm/internal/store"
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

// bareEmail strips a "Name <addr>" wrapper down to the address.
func bareEmail(a string) string {
	if i := strings.LastIndex(a, "<"); i >= 0 {
		return strings.TrimSpace(strings.TrimSuffix(a[i+1:], ">"))
	}
	return strings.TrimSpace(a)
}

// accountForAddress finds the account that owns a from-address — its primary
// Email or one of its aliases — matched case-insensitively on the bare address.
func (s *Server) accountForAddress(addr string) (config.Account, bool) {
	want := strings.ToLower(bareEmail(addr))
	if want == "" {
		return config.Account{}, false
	}
	for _, a := range s.cfg.Accounts {
		if strings.ToLower(a.Email) == want {
			return a, true
		}
		for _, al := range a.Aliases {
			if strings.ToLower(bareEmail(al.Email)) == want {
				return a, true
			}
		}
	}
	return config.Account{}, false
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
	InReplyTo     string // original Message-ID, for the In-Reply-To header
	References    string // full References header value to reuse
	ThreadContext string // for the embedded ai_reply partial
	UndoSendDelay int    // seconds the split-button "Send" waits (undo window)
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
	}
	if len(s.cfg.Accounts) > 0 {
		view.Account = s.cfg.Accounts[0].Name
		view.From = s.cfg.Accounts[0].Email
	}
	// Reopening a saved local draft (from /drafts) just loads its fields —
	// no new row, no reply prefill.
	if draftID, err := strconv.ParseInt(r.URL.Query().Get("draft"), 10, 64); err == nil && draftID > 0 {
		if d, err := s.store.DraftByID(draftID); err == nil && d != nil {
			refs := ""
			if d.InReplyTo != "" {
				// ponytail: a reopened draft only remembers its immediate
				// parent, not the full References chain — still threads
				// correctly via In-Reply-To in every mail client that matters.
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
			s.renderCompose(w, composeView{
				CSRF: view.CSRF, Accounts: view.Accounts, DraftID: d.ID, Account: d.Account, From: from,
				To: d.To, Cc: d.Cc, Bcc: d.Bcc, Subject: d.Subject, Body: d.Body, Mode: mode,
				Kind: d.Kind, InReplyTo: d.InReplyTo, References: refs, UndoSendDelay: view.UndoSendDelay,
			})
			return
		}
	}
	if replyID, err := strconv.ParseInt(r.URL.Query().Get("reply"), 10, 64); err == nil && replyID > 0 {
		if orig, err := s.store.MessageByID(replyID); err == nil && orig != nil {
			s.prefillReply(&view, orig, r.URL.Query().Get("mode"))
		}
	}
	// Fresh open (new/reply/forward): the client auto-inserts the linked signature.
	view.AutoSignature = true
	s.renderCompose(w, view)
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
	// ponytail: quotes the stored snippet (first ~2KB), not the full fetched
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
func (s *Server) handleComposeDraftSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
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
	if id == 0 && d.ID != 0 {
		// First save: tell the open form its new draft id via OOB swap so later
		// saves target this row instead of creating more.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<input type="hidden" id="compose-draft-id" name="draft_id" value="%d" hx-swap-oob="true">`, d.ID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleComposePreview renders markdown source (POST body field) to the same
// sanitized HTML used at send time, for the markdown editor's Preview tab.
func (s *Server) handleComposePreview(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// #nosec G705 -- RenderMarkdown escapes raw HTML (goldmark default) and runs
	// the bluemonday emailPolicy on its output; the fragment is sanitized.
	_, _ = io.WriteString(w, mail.RenderMarkdown(r.PostFormValue("body")))
}

// maxAttachTotal caps the combined size of all attachments on one message.
const maxAttachTotal = 25 << 20 // 25MB

// readAttachments pulls the uploaded "attachments" files off a parsed multipart
// form, enforcing the total size cap. The second return is a user-facing error
// message ("" on success). Returns nil for a non-multipart request (no files).
// Draft-attachment persistence is intentionally not implemented — attachments
// only ride along with the outgoing send.
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
	multipart := strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data")
	if multipart {
		// Hard cap the whole request so a giant upload can't exhaust disk;
		// the per-file sum below gives the clean 25MB error before this trips.
		r.Body = http.MaxBytesReader(w, r.Body, maxAttachTotal+(1<<20))
		// #nosec G120 -- body is capped by MaxBytesReader above
		if err := r.ParseMultipartForm(maxAttachTotal + (1 << 20)); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
	} else if err := r.ParseForm(); err != nil {
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
	atts, attErr := readAttachments(r)
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
		if draftID > 0 {
			_ = s.store.DeleteDraft(draftID)
		}
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
	if draftID > 0 {
		_ = s.store.DeleteDraft(draftID)
	}
	if scheduled {
		w.Header().Set("SM-Scheduled", sendAt.UTC().Format(time.RFC3339))
	} else {
		w.Header().Set("SM-Outbox-Id", strconv.FormatInt(o.ID, 10))
	}
	w.WriteHeader(http.StatusNoContent)
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
// window with the draft restored (attachments excepted — they can't be put back
// into a file input). Wired to the toast "Undo" action.
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
		UndoSendDelay: prefs.UndoSendDelay,
	})
}

// --- local drafts page (the simpler alternative to folding drafts into the
// IMAP Drafts special-use folder view — see server.go routes) ---

func (s *Server) handleDraftsPage(w http.ResponseWriter, r *http.Request) {
	drafts, err := s.store.ListDrafts()
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
	if err := s.store.DeleteDraft(id); err != nil {
		slog.Error("drafts: delete", "err", err)
	}
	http.Redirect(w, r, "/drafts", http.StatusSeeOther)
}
