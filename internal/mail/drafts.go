// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	emmail "github.com/emersion/go-message/mail"

	"github.com/mattmezza/mimux/internal/store"
)

// draftsFolderName is the mailbox mimux creates for an account that has none.
// Top-level, so the server's hierarchy delimiter never enters into it.
const draftsFolderName = "Drafts"

// INVARIANT, same one the flag mutations in actions.go keep: the SQLite row is
// written FIRST and this runs afterwards, in the background. A draft is the one
// thing a mail client must not lose, so an unreachable server, an expired
// token, or a mailbox that refuses CREATE can only ever delay publication — the
// row is already saved, imap_dirty is already set, and pushDirtyDrafts retries
// every sync cycle until it lands.

// PushDraft publishes a saved draft to the account's IMAP Drafts folder so it
// follows the user to their phone. Every save appends a whole new revision and
// then expunges the one it replaces — IMAP has no "edit message" — which is why
// the ordering below is append-then-delete: interrupted halfway, the mailbox
// holds a duplicate revision, never a hole where the draft used to be.
func (m *Manager) PushDraft(ctx context.Context, d *store.Draft) error {
	return m.pushDraft(ctx, nil, d)
}

// pushDraft is PushDraft on the caller's own connection (see account.exec);
// nil c queues it for the worker, which is what an HTTP handler wants.
func (m *Manager) pushDraft(ctx context.Context, c *imapclient.Client, d *store.Draft) error {
	a := m.account(d.Account)
	if a == nil {
		return fmt.Errorf("unknown account %q", d.Account)
	}
	in := ComposeInput{
		To: SplitAddrList(d.To), Cc: SplitAddrList(d.Cc), Bcc: SplitAddrList(d.Bcc),
		Subject: d.Subject, Body: d.Body, Mode: d.Mode, InReplyTo: d.InReplyTo,
		MessageID: d.MessageID, KeepBcc: true,
	}
	if d.InReplyTo != "" {
		in.References = "<" + d.InReplyTo + ">"
	}
	// The draft's files go up with it, so the copy on the phone is the whole
	// unfinished message. BuildMessage already multiparts for the send path.
	atts, err := m.st.DraftAttachments(d.ID)
	if err != nil {
		return fmt.Errorf("draft attachments: %w", err)
	}
	for _, at := range atts {
		in.Attachments = append(in.Attachments, OutAttachment{
			Filename: at.Filename, ContentType: at.ContentType, Data: at.Data,
		})
	}
	raw, msgID, err := BuildMessage(a.cfg, in, d.UpdatedAt)
	if err != nil {
		return fmt.Errorf("build draft: %w", err)
	}
	return a.exec(ctx, c, func(c *imapclient.Client) error {
		f, err := a.draftsFolder(c)
		if err != nil || f == nil {
			return err
		}
		// \Seen along with \Draft: an unread badge counting the user's own
		// half-written replies is noise, and RecountUnread would show it.
		cmd := c.Append(f.Name, int64(len(raw)), &imap.AppendOptions{
			Flags: []imap.Flag{imap.FlagDraft, imap.FlagSeen},
		})
		if _, err := cmd.Write(raw); err != nil {
			_ = cmd.Close()
			return err
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		data, err := cmd.Wait()
		if err != nil {
			return err
		}
		var newUID imap.UID
		if data != nil {
			newUID = data.UID // APPENDUID; 0 without UIDPLUS
		}
		// Only now that the new revision is safely on the server does the old
		// one go.
		if d.UID > 0 && d.FolderID != 0 {
			if old, err := m.st.FolderByID(d.FolderID); err == nil && old != nil {
				gone, err := dropRevision(c, old.Name, imap.UID(d.UID))
				if err != nil {
					slog.Warn("drafts: could not remove the previous revision",
						"account", d.Account, "draft", d.ID, "err", err)
				}
				if gone {
					_ = m.st.DeleteMessageByUID(old.ID, d.UID)
				}
			}
		}
		changed, err := a.syncFolder(ctx, c, f, c.Caps())
		if err != nil {
			return err
		}
		if changed {
			a.signalListChanged()
		}
		if newUID == 0 {
			// No APPENDUID: the sync above has just stored the copy we appended,
			// so the Message-ID we stamped on it finds the UID.
			u, err := m.st.MessageUIDByMessageID(f.ID, msgID)
			if err != nil {
				return err
			}
			newUID = imap.UID(u)
		}
		return m.st.ClearDraftDirty(d.ID, msgID, f.ID, uint32(newUID), d.UpdatedAt) // #nosec G115 -- UID fits uint32 by protocol
	})
}

// AdoptDraft makes a draft written in another client editable here, by giving
// it the local row it never had: the message is fetched, parsed back into
// compose fields and attachments, and stored seeded with the Message-ID, folder
// and UID of the copy it came from. That seeding is the whole trick — the first
// save then goes down pushDraft's replace path and expunges the copy it came
// from, instead of appending a second draft beside it.
//
// Idempotent: opening the same foreign draft twice returns the row the first
// open created, so nobody ends up with two copies of one unfinished message.
// An error means the draft could not be brought over faithfully (see
// parseDraftMessage) and the caller should leave it read-only.
func (m *Manager) AdoptDraft(ctx context.Context, msg *store.Message) (*store.Draft, error) {
	return m.adoptDraft(ctx, nil, msg)
}

// adoptDraft is AdoptDraft on the caller's own connection (see fetchRawOn).
func (m *Manager) adoptDraft(ctx context.Context, c *imapclient.Client, msg *store.Message) (*store.Draft, error) {
	existing, err := m.st.DraftByServerCopy(msg.Account, msg.MessageID, msg.FolderID, msg.UID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	raw, err := m.fetchRawOn(ctx, c, msg)
	if err != nil {
		return nil, err
	}
	d, atts, err := parseDraftMessage(raw)
	if err != nil {
		return nil, err
	}
	d.Account = msg.Account
	if err := m.st.UpsertDraft(d); err != nil {
		return nil, err
	}
	for i := range atts {
		if err := m.st.AddDraftAttachment(d.ID, &atts[i]); err != nil {
			slog.Warn("drafts: attachment not kept while adopting",
				"account", msg.Account, "draft", d.ID, "err", err)
		}
	}
	// ClearDraftDirty says exactly what is true here: this content is on the
	// server, at this location. It also clears the push debt UpsertDraft just
	// took on — nothing has been edited yet, so republishing an identical copy
	// would only churn the UID.
	if err := m.st.ClearDraftDirty(d.ID, msg.MessageID, msg.FolderID, msg.UID, d.UpdatedAt); err != nil {
		return nil, err
	}
	d.MessageID, d.FolderID, d.UID, d.IMAPDirty = msg.MessageID, msg.FolderID, msg.UID, false
	return d, nil
}

// parseDraftMessage turns a raw draft message into the compose fields a local
// draft row holds, plus its attachments: the inverse of pushDraft, used to read
// back a draft written by another client (and, in the tests, our own).
//
// The body mapping is deliberate. A text/html draft becomes Mode "html" with
// the fragment run through SanitizeComposeHTML — the very same allowlist the
// WYSIWYG editor's output goes through on the way out, never the reading-pane
// sanitizer, whose cid: rewriting and external-image blocking are for display
// and would be saved back as the user's own markup. What comes out is therefore
// what the editor would have produced, and saving it re-sanitizes to the same
// thing. Everything else is Mode "plain": markdown leaves no trace in a sent
// message, so a draft is never guessed to be markdown — its source would be
// unrecoverable and the guess would rewrite the user's text.
//
// An error means "this cannot be edited without damaging it": the caller falls
// back to the read-only view rather than adopting something lossy.
func parseDraftMessage(raw []byte) (*store.Draft, []store.DraftAttachment, error) {
	mr, err := emmail.CreateReader(bytes.NewReader(raw))
	if mr == nil {
		return nil, nil, fmt.Errorf("parse draft: %w", err)
	}
	defer func() { _ = mr.Close() }()
	if ct, _, _ := mr.Header.ContentType(); !editableDraftType(ct) {
		return nil, nil, fmt.Errorf("draft is %s: editing it here would break it", ct)
	}

	d := &store.Draft{Subject: headerSubject(mr.Header), Kind: "new", Mode: "plain"}
	d.To = joinAddresses(mr.Header, "To")
	d.Cc = joinAddresses(mr.Header, "Cc")
	d.Bcc = joinAddresses(mr.Header, "Bcc")
	if irt, err := mr.Header.MsgIDList("In-Reply-To"); err == nil && len(irt) > 0 {
		d.InReplyTo, d.Kind = irt[0], "reply"
	}

	var text, htmlBody string
	var atts []store.DraftAttachment
	budget := int64(MaxAttachTotal)
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil && !message.IsUnknownCharset(err) {
			return nil, nil, fmt.Errorf("parse draft part: %w", err)
		}
		// A cid: part is an inline image the compose editor cannot author or
		// re-attach, and the body references it by id. Editing would leave a
		// dangling image, so this draft stays read-only.
		if p.Header.Get("Content-ID") != "" {
			return nil, nil, fmt.Errorf("draft has inline images that compose cannot reproduce")
		}
		// mail.PartHeader is a four-method interface; the two concrete headers
		// behind it both embed message.Header, which is where the parsed
		// Content-* accessors live.
		mh, ok := p.Header.(interface {
			ContentType() (string, map[string]string, error)
			ContentDisposition() (string, map[string]string, error)
		})
		if !ok {
			return nil, nil, fmt.Errorf("draft part has no readable content type")
		}
		mt, _, _ := mh.ContentType()
		mt = strings.ToLower(mt)
		_, dparams, _ := mh.ContentDisposition()
		body, err := io.ReadAll(io.LimitReader(p.Body, budget+1))
		if err != nil {
			return nil, nil, fmt.Errorf("read draft part: %w", err)
		}
		inline := false
		if _, ok := p.Header.(*emmail.InlineHeader); ok {
			inline = dparams["filename"] == ""
		}
		switch {
		case inline && mt == "text/html" && htmlBody == "":
			htmlBody = string(body)
		case inline && (mt == "text/plain" || mt == "") && text == "":
			text = string(body)
		case inline && strings.HasPrefix(mt, "text/"):
			// A second text part of a kind compose has no editor for (a duplicate
			// alternative, text/enriched): ignore it rather than attach it.
		default:
			budget -= int64(len(body))
			if budget < 0 {
				return nil, nil, fmt.Errorf("draft attachments exceed the %dMB limit", MaxAttachTotal>>20)
			}
			atts = append(atts, store.DraftAttachment{
				Filename: partFilename(p.Header, dparams), ContentType: mt, Data: body,
			})
		}
	}

	if safe := SanitizeComposeHTML(htmlBody); strings.TrimSpace(safe) != "" {
		d.Mode, d.Body = "html", safe
	} else {
		// No HTML part, or one that is nothing but markup compose cannot keep:
		// the plain text alternative is the honest version of it.
		d.Body = text
	}
	if d.Subject == "" && d.To == "" && d.Cc == "" && strings.TrimSpace(d.Body) == "" && len(atts) == 0 {
		return nil, nil, fmt.Errorf("nothing readable in this draft")
	}
	return d, atts, nil
}

// editableDraftType rejects the message shapes whose whole point is the
// envelope mimux would throw away by re-composing them.
func editableDraftType(contentType string) bool {
	switch strings.ToLower(contentType) {
	case "multipart/encrypted", "multipart/signed",
		"application/pkcs7-mime", "application/pgp-encrypted":
		return false
	}
	return true
}

func headerSubject(h emmail.Header) string {
	s, err := h.Subject()
	if err != nil {
		return h.Get("Subject")
	}
	return s
}

// joinAddresses renders one address header the way compose stores and shows a
// recipient list: "Name <addr>" entries joined by ", ". An unparseable header is
// kept verbatim rather than dropped — losing recipients is the worse failure.
func joinAddresses(h emmail.Header, key string) string {
	list, err := h.AddressList(key)
	if err != nil || len(list) == 0 {
		return strings.TrimSpace(h.Get(key))
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		// Bare address when there is no display name: that is how compose holds
		// what the user typed, and "<a@b>" in the To field is noise.
		if a.Name == "" {
			out = append(out, a.Address)
			continue
		}
		out = append(out, a.String())
	}
	return strings.Join(out, ", ")
}

func partFilename(h emmail.PartHeader, dparams map[string]string) string {
	if ah, ok := h.(*emmail.AttachmentHeader); ok {
		if fn, err := ah.Filename(); err == nil && fn != "" {
			return fn
		}
	}
	if fn := dparams["filename"]; fn != "" {
		return fn
	}
	return "attachment"
}

// DropDraft removes a draft's published revision from the Drafts folder: the
// draft was sent, or deleted. A draft that never reached the server has nothing
// to remove. Best-effort by design — the local row is going either way, and a
// leftover copy is a stale draft, not lost mail.
func (m *Manager) DropDraft(ctx context.Context, d *store.Draft) error {
	if d == nil || d.UID == 0 || d.FolderID == 0 {
		return nil
	}
	a := m.account(d.Account)
	if a == nil {
		return nil
	}
	f, err := m.st.FolderByID(d.FolderID)
	if err != nil || f == nil {
		return err
	}
	return a.submit(ctx, func(c *imapclient.Client) error {
		gone, err := dropRevision(c, f.Name, imap.UID(d.UID))
		if err != nil {
			return err
		}
		if gone {
			_ = m.st.DeleteMessageByUID(f.ID, d.UID)
			a.signalListChanged()
		}
		return nil
	})
}

// dropRevision marks one draft copy \Deleted and, with UIDPLUS, expunges just
// it — reporting whether the copy is actually gone from the server, so the
// local row is only dropped when it is. Without UIDPLUS the flag is where this
// stops: a plain EXPUNGE would also remove whatever another client left marked
// deleted in that mailbox, so the copy stays visible-but-deleted rather than
// taking someone else's mail with it. (moveTo accepts that trade because it is
// holding the message it moves; here there is nothing to lose by waiting for a
// UIDPLUS server.)
func dropRevision(c *imapclient.Client, folder string, uid imap.UID) (gone bool, err error) {
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		return false, err
	}
	set := imap.UIDSet{}
	set.AddNum(uid)
	if err := c.Store(set, &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close(); err != nil {
		return false, err
	}
	if !c.Caps().Has(imap.CapUIDPlus) {
		return false, nil
	}
	if err := c.UIDExpunge(set).Close(); err != nil {
		return false, err
	}
	return true, nil
}

// draftsFolder returns the account's Drafts folder, creating it when the
// account has none — the drafts have to go somewhere, and an account without a
// Drafts mailbox is otherwise silently stuck local-only. Rediscovery after the
// CREATE is what gives the row its "drafts" special-use (the name heuristic in
// detectSpecialUse catches "Drafts"); syncFolders LISTs "*" rather than
// subscribed mailboxes, so no SUBSCRIBE is needed for it to be found again.
// A server that refuses the CREATE (read-only account, ACL) gets a log line and
// nil: the draft stays saved locally, which is the whole point of pushing last.
func (a *account) draftsFolder(c *imapclient.Client) (*store.Folder, error) {
	f, err := a.m.st.FolderBySpecial(a.cfg.Name, "drafts")
	if err != nil || f != nil {
		return f, err
	}
	if err := c.Create(draftsFolderName, nil).Wait(); err != nil {
		slog.Warn("drafts: no Drafts folder and the server would not create one",
			"account", a.cfg.Name, "err", err)
		return nil, nil
	}
	if _, err := a.syncFolders(c); err != nil {
		return nil, err
	}
	return a.m.st.FolderBySpecial(a.cfg.Name, "drafts")
}

// pushDirtyDrafts republishes every draft this account saved locally but never
// got onto the server: a push that errored, one that hit submit's budget while
// the worker was reconnecting, one still owed when the process restarted, or —
// on the first cycle after migration 0230 — every draft written before drafts
// were an IMAP thing at all. Runs on the worker's own connection at the top of
// each sync cycle, beside pushSeenDirty.
func (a *account) pushDirtyDrafts(ctx context.Context, c *imapclient.Client) {
	drafts, err := a.m.st.DirtyDrafts(a.cfg.Name, 50)
	if err != nil {
		return
	}
	for i := range drafts {
		if err := a.m.pushDraft(ctx, c, &drafts[i]); err != nil {
			slog.Warn("drafts: push still owed", "account", a.cfg.Name, "draft", drafts[i].ID, "err", err)
			return // connection is unhappy: leave the rest owed, next cycle retries
		}
	}
}
