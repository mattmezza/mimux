// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

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
		MessageID: d.MessageID,
	}
	if d.InReplyTo != "" {
		in.References = "<" + d.InReplyTo + ">"
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
