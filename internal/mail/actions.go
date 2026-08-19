// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/mimux/internal/store"
)

// Body returns the sanitized HTML document for a message. The parsed body is
// cached (in-memory LRU, then SQLite); force bypasses both caches to re-fetch
// from the IMAP server and overwrite them. blockedExternal reports whether
// external images were suppressed. render(allowExternal) runs per call so the
// external-image toggle keeps working off a cached body.
func (m *Manager) Body(ctx context.Context, msg *store.Message, allowExternal, force bool) (out string, blockedExternal bool, err error) {
	body, err := m.parsedBody(ctx, msg, force)
	if err != nil {
		return "", false, err
	}
	out, blocked := body.render(allowExternal)
	return out, blocked, nil
}

// parsedBody resolves a message's parsed body via LRU → SQLite → IMAP, caching
// the result in both on a miss. force skips both caches, re-fetches, and
// overwrites them.
func (m *Manager) parsedBody(ctx context.Context, msg *store.Message, force bool) (*messageBody, error) {
	if !force {
		if b, ok := m.bodies.get(msg.ID); ok {
			return b, nil
		}
		if blob, ok, _ := m.st.GetMessageBody(msg.ID); ok {
			if b, err := decodeBody(blob); err == nil {
				m.bodies.put(msg.ID, b)
				return b, nil
			}
			// corrupt/incompatible blob: fall through and re-fetch.
		}
	}
	raw, err := m.fetchRaw(ctx, msg)
	if err != nil {
		return nil, err
	}
	body := parseBody(raw)
	if blob, err := encodeBody(body); err == nil {
		_ = m.st.SaveMessageBody(msg.ID, blob) // best-effort cache
	}
	m.bodies.put(msg.ID, body)
	return body, nil
}

func (m *Manager) fetchRaw(ctx context.Context, msg *store.Message) ([]byte, error) {
	a := m.accounts[msg.Account]
	if a == nil {
		return nil, fmt.Errorf("unknown account %q", msg.Account)
	}
	f, err := m.st.FolderByID(msg.FolderID)
	if err != nil || f == nil {
		return nil, fmt.Errorf("folder not found")
	}
	var raw []byte
	err = a.submitRO(ctx, func(c *imapclient.Client) error {
		if _, err := c.Select(f.Name, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return err
		}
		set := imap.UIDSet{}
		set.AddNum(imap.UID(msg.UID))
		data, err := c.Fetch(set, &imap.FetchOptions{
			BodySection: []*imap.FetchItemBodySection{{Peek: true}},
		}).Collect()
		if err != nil {
			return err
		}
		if len(data) == 0 || len(data[0].BodySection) == 0 {
			return fmt.Errorf("empty body")
		}
		raw = data[0].BodySection[0].Bytes
		return nil
	})
	return raw, err
}

// Who made a change, for the message.updated payload's "origin" field. It is
// there for troubleshooting: "my rule fired twice" and "another client is
// fighting me over this flag" look identical without it.
const (
	originMimux    = "mimux"
	originExternal = "external"
)

// updated announces a change to a stored message: the store id, one word for
// what changed, and who changed it ("123 starred mimux"). Published by the four
// mutation helpers below and by the flag sync (fetchFlagChanges) for a change
// another IMAP client made; the browser ignores it (it refreshes off new-mail)
// and pro/webhooks.go turns it into a message.updated delivery.
//
// The guard is signalNewMessage's: these helpers are not only user/API-driven,
// filter rules run them from inside the sync loop for every newly-stored
// message (see applyAction), which on a first sync is the whole mailbox.
// LastSync stays zero for that entire first cycle, so backfill is silent and
// steady-state rule hits are not.
//
// ponytail: a mid-session UIDVALIDITY re-fetch re-runs the rules over old mail
// with LastSync already set, so that one path can still burst. Thread a "this
// is the sync worker" flag through applyAction if it ever bites.
func (m *Manager) updated(msg *store.Message, change, origin string) {
	a := m.account(msg.Account)
	if a == nil || a.getStatus().LastSync.IsZero() {
		return
	}
	m.hub.broadcast(Event{Type: "message-updated",
		Data: strconv.FormatInt(msg.ID, 10) + " " + change + " " + origin})
}

// changeWord names a boolean change for the message-updated event.
func changeWord(on bool, yes, no string) string {
	if on {
		return yes
	}
	return no
}

// INVARIANT for every mutation below: the store is written FIRST (by the
// handler, the API or the filter rule), and the IMAP push happens afterwards,
// in the background. That ordering is what makes the sync loop's external-change
// detection safe — when the server echoes our own flag back, fetchFlagChanges
// writes a value the row already has, the setter reports "nothing moved", and no
// event fires. A caller that pushed to IMAP before writing the store would
// manufacture a self-inflicted "external" change on the very next sync.

// SetRead adds or removes the \Seen flag on the server and, once it lands,
// clears the store's "push still owed" marker that store.SetRead set. If the
// push fails the marker stays and pushSeenDirty retries it every sync cycle —
// callers fire this in the background, so a dropped error used to mean the flag
// silently never reached the server.
func (m *Manager) SetRead(ctx context.Context, msg *store.Message, read bool) error {
	return m.setRead(ctx, nil, msg, read)
}

// setRead is SetRead with the caller's own connection (see account.exec); nil c
// means "queue it for the worker", which is what every caller but a filter rule
// wants.
func (m *Manager) setRead(ctx context.Context, c *imapclient.Client, msg *store.Message, read bool) error {
	if err := m.storeFlag(ctx, c, msg, imap.FlagSeen, read); err != nil {
		return err
	}
	defer m.changed(msg)
	m.updated(msg, changeWord(read, "read", "unread"), originMimux)
	return m.st.ClearSeenDirty(msg.ID, read)
}

// pushSeenDirty re-pushes every \Seen flag this account flipped locally but
// never confirmed on the server: a push that errored, one that hit submit's
// 30s budget while the worker was reconnecting, or one still queued when the
// process restarted. It runs on the worker's own connection at the top of each
// sync cycle, before the flag sync reads the server's truth back.
//
// NOTE: one SELECT per row and no batching — the set is empty in the steady
// state and a handful otherwise. Group by folder if a wedged account ever makes
// this show up. Reusing store.Outbox was the other option and doesn't fit: its
// columns are a compose payload and the scheduler drains it into Manager.Send.
func (a *account) pushSeenDirty(c *imapclient.Client) {
	msgs, err := a.m.st.DirtySeen(a.cfg.Name, 200)
	if err != nil || len(msgs) == 0 {
		return
	}
	for i := range msgs {
		msg := &msgs[i]
		f, err := a.m.st.FolderByID(msg.FolderID)
		if err != nil || f == nil {
			continue
		}
		if _, err := c.Select(f.Name, nil).Wait(); err != nil {
			return // connection is unhappy: leave the rest owed, next cycle retries
		}
		op := imap.StoreFlagsAdd
		if !msg.IsRead {
			op = imap.StoreFlagsDel
		}
		set := imap.UIDSet{}
		set.AddNum(imap.UID(msg.UID))
		if err := c.Store(set, &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close(); err != nil {
			return
		}
		_ = a.m.st.ClearSeenDirty(msg.ID, msg.IsRead)
	}
}

// SetStarred adds or removes the \Flagged flag on the server.
func (m *Manager) SetStarred(ctx context.Context, msg *store.Message, starred bool) error {
	return m.setStarred(ctx, nil, msg, starred)
}

func (m *Manager) setStarred(ctx context.Context, c *imapclient.Client, msg *store.Message, starred bool) error {
	if err := m.storeFlag(ctx, c, msg, imap.FlagFlagged, starred); err != nil {
		return err
	}
	m.changed(msg)
	m.updated(msg, changeWord(starred, "starred", "unstarred"), originMimux)
	return nil
}

func (m *Manager) storeFlag(ctx context.Context, c *imapclient.Client, msg *store.Message, flag imap.Flag, add bool) error {
	a := m.accounts[msg.Account]
	if a == nil {
		return fmt.Errorf("unknown account %q", msg.Account)
	}
	f, err := m.st.FolderByID(msg.FolderID)
	if err != nil || f == nil {
		return fmt.Errorf("folder not found")
	}
	op := imap.StoreFlagsAdd
	if !add {
		op = imap.StoreFlagsDel
	}
	return a.exec(ctx, c, func(c *imapclient.Client) error {
		if _, err := c.Select(f.Name, nil).Wait(); err != nil {
			return err
		}
		set := imap.UIDSet{}
		set.AddNum(imap.UID(msg.UID))
		return c.Store(set, &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{flag}}, nil).Close()
	})
}

// SetLabel adds (add) or removes a user label on a message and persists the
// new space-joined value, updating msg in place. It is the one place labels
// are written — the reading pane, the row menu and the "label" filter action
// all come through here.
//
// NOTE: local-only, no server round-trip. Unlike \Seen/\Flagged there is
// no cheap IMAP call to make: go-imap/v2 beta.8 (newest tag, and upstream
// master too) can neither FETCH nor STORE Gmail's X-GM-LABELS — its encoders
// reject the unknown atom and the response parser errors on it, with no raw
// command escape hatch. So a label applied here never reaches gmail.com and a
// label applied in gmail.com never reaches here. Give this the storeFlag
// treatment (an a.submit STORE, backgrounded by the caller) the moment the
// library ships Gmail-extension support.
func (m *Manager) SetLabel(msg *store.Message, label string, add bool) error {
	labels := RemoveLabel(msg.Labels, label)
	if add {
		labels = AddLabel(msg.Labels, label)
	}
	if labels == msg.Labels {
		return nil
	}
	msg.Labels = labels
	if _, err := m.st.SetLabels(msg.ID, labels); err != nil {
		return err
	}
	m.changed(msg)
	m.updated(msg, changeWord(add, "labeled", "unlabeled"), originMimux)
	return nil
}

// Move relocates a message to the account's folder with the given special-use
// role (trash/archive/spam), via MOVE or COPY+\Deleted+EXPUNGE fallback.
func (m *Manager) Move(ctx context.Context, msg *store.Message, targetSpecial string) error {
	target, err := m.st.FolderBySpecial(msg.Account, targetSpecial)
	if err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("no %s folder for this account", targetSpecial)
	}
	return m.moveTo(ctx, nil, msg, target)
}

// MoveToFolder is Move's named-folder variant, for filter rule "move"
// actions: name resolves against the account's special-use folders first
// (so "archive"/"trash"/... keep working), then against literal folder
// names.
func (m *Manager) MoveToFolder(ctx context.Context, msg *store.Message, name string) error {
	return m.moveToFolder(ctx, nil, msg, name)
}

// moveToFolder is MoveToFolder with the caller's own connection (account.exec).
func (m *Manager) moveToFolder(ctx context.Context, c *imapclient.Client, msg *store.Message, name string) error {
	target, err := m.st.FolderBySpecial(msg.Account, strings.ToLower(name))
	if err != nil {
		return err
	}
	if target == nil {
		if target, err = m.st.FolderByName(msg.Account, name); err != nil {
			return err
		}
	}
	if target == nil {
		return fmt.Errorf("no folder named %q for account %q", name, msg.Account)
	}
	return m.moveTo(ctx, c, msg, target)
}

func (m *Manager) moveTo(ctx context.Context, c *imapclient.Client, msg *store.Message, target *store.Folder) error {
	a := m.accounts[msg.Account]
	if a == nil {
		return fmt.Errorf("unknown account %q", msg.Account)
	}
	src, err := m.st.FolderByID(msg.FolderID)
	if err != nil || src == nil {
		return fmt.Errorf("folder not found")
	}
	if target.ID == src.ID {
		return nil
	}
	defer m.changed(msg)
	var destUID imap.UID
	if err := a.exec(ctx, c, func(c *imapclient.Client) error {
		if _, err := c.Select(src.Name, nil).Wait(); err != nil {
			return err
		}
		set := imap.UIDSet{}
		set.AddNum(imap.UID(msg.UID))
		if c.Caps().Has(imap.CapMove) {
			data, err := c.Move(set, target.Name).Wait()
			if err == nil && data != nil {
				destUID = onlyUID(data.DestUIDs)
			}
			return err
		}
		data, err := c.Copy(set, target.Name).Wait()
		if err != nil {
			return err
		}
		if data != nil {
			destUID = onlyUID(data.DestUIDs)
		}
		if err := c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
			return err
		}
		if c.Caps().Has(imap.CapUIDPlus) {
			return c.UIDExpunge(set).Close()
		}
		return c.Expunge().Close()
	}); err != nil {
		return err
	}
	// The row's UID belonged to the source folder and means nothing in the
	// destination — it is why an optimistically moved message could not have its
	// body fetched, and why expunge reconciliation would read it as gone. COPYUID
	// tells us the real one; without UIDPLUS there is nothing to correct it with,
	// so the row goes and the destination's next sync brings the message back
	// with a UID that works.
	if destUID > 0 {
		_ = m.st.SetMessageLocation(msg.ID, target.ID, uint32(destUID))
		msg.FolderID, msg.UID = target.ID, uint32(destUID)
		m.updated(msg, "moved", originMimux)
		return nil
	}
	_ = m.st.DeleteMessage(msg.ID)
	return nil
}

// onlyUID returns the single UID in a COPYUID/MOVE destination set, or 0 when
// the server did not report one (no UIDPLUS) — mimux moves one message at a
// time, so anything else is a server being creative and not worth trusting.
func onlyUID(set imap.NumSet) imap.UID {
	uids, ok := set.(imap.UIDSet)
	if !ok {
		return 0
	}
	nums, ok := uids.Nums()
	if !ok || len(nums) != 1 {
		return 0
	}
	return nums[0]
}

// changed announces that a message this user acted on is no longer what the
// open pages are showing. It reuses new-mail — "the list moved, re-read it" —
// because that is exactly what every consumer of it already does, and it is the
// only way a second tab or a phone finds out before the next sync poll.
//
// The tab that asked for the change also refreshes. That is accepted: the swap
// it already did and the refresh it now does render the same list. Deferred at
// the read and move call sites, so a failed action also announces itself — a
// re-read is what puts an optimistic swap back the way the server sees it.
//
// ponytail: one broadcast per mutated message. A select-many path (bulk mark
// read) wants these coalesced per request — do it there, by broadcasting once
// after the loop, when that path exists.
func (m *Manager) changed(msg *store.Message) {
	m.hub.broadcast(Event{Type: "new-mail", Data: msg.Account})
}

// Toast pushes a transient error message to connected clients.
func (m *Manager) Toast(text string) { m.hub.broadcast(Event{Type: "toast", Data: text}) }
