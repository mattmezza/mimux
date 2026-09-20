// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"

	"github.com/mattmezza/mimux/internal/store"
)

// specialSort orders folders in the sidebar: special-use first, then the rest.
var specialSort = map[string]int{
	"inbox": 0, "sent": 1, "drafts": 2, "archive": 3, "spam": 4, "trash": 5,
}

func sortForSpecial(s string) int {
	if v, ok := specialSort[s]; ok {
		return v
	}
	return 10
}

// detectSpecialUse maps IMAP SPECIAL-USE attributes (preferred) or a
// case-insensitive name heuristic (fallback) to one of our six roles.
func detectSpecialUse(attrs []imap.MailboxAttr, name string) string {
	for _, at := range attrs {
		switch at {
		case imap.MailboxAttrSent:
			return "sent"
		case imap.MailboxAttrDrafts:
			return "drafts"
		case imap.MailboxAttrTrash:
			return "trash"
		case imap.MailboxAttrJunk:
			return "spam"
		case imap.MailboxAttrArchive:
			return "archive"
		}
	}
	// Name heuristic. Compare the leaf name case-insensitively.
	leaf := name
	if i := strings.LastIndexAny(name, "/."); i >= 0 {
		leaf = name[i+1:]
	}
	switch strings.ToLower(strings.TrimSpace(leaf)) {
	case "inbox":
		return "inbox"
	case "sent", "sent items", "sent mail", "sent messages":
		return "sent"
	case "drafts", "draft":
		return "drafts"
	case "trash", "deleted", "deleted items", "deleted messages", "bin":
		return "trash"
	case "junk", "junk email", "spam":
		return "spam"
	case "archive", "archives", "all mail", "archived":
		return "archive"
	}
	if strings.EqualFold(name, "INBOX") {
		return "inbox"
	}
	return ""
}

// syncFolders LISTs the account's mailboxes and upserts them, returning the
// stored folder rows.
func (a *account) syncFolders(c *imapclient.Client) ([]store.Folder, error) {
	opts := &imap.ListOptions{}
	if c.Caps().Has(imap.CapSpecialUse) {
		opts.ReturnSpecialUse = true
	}
	list, err := c.List("", "*", opts).Collect()
	if err != nil {
		return nil, err
	}
	for _, m := range list {
		if hasAttr(m.Attrs, imap.MailboxAttrNonExistent) || hasAttr(m.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}
		special := detectSpecialUse(m.Attrs, m.Mailbox)
		if _, err := a.m.st.UpsertFolder(a.cfg.Name, m.Mailbox, special, sortForSpecial(special)); err != nil {
			return nil, err
		}
	}
	return a.m.st.ListFolders(a.cfg.Name)
}

func hasAttr(attrs []imap.MailboxAttr, want imap.MailboxAttr) bool {
	for _, at := range attrs {
		if at == want {
			return true
		}
	}
	return false
}

// syncUIDValidity resets a folder's cache when the server's UIDVALIDITY changes,
// then records the current value. Pure and store-only so it can be tested
// without a live IMAP server.
func syncUIDValidity(st *store.Store, f *store.Folder, serverUV uint32) (reset bool, err error) {
	if f.UIDValidity != 0 && f.UIDValidity != serverUV {
		if err := st.ClearFolderMessages(f.ID); err != nil {
			return false, err
		}
		if err := st.SetFolderModSeq(f.ID, 0); err != nil {
			return false, err
		}
		f.HighestModSeq = 0
		reset = true
	}
	if f.UIDValidity != serverUV {
		if err := st.SetFolderUIDValidity(f.ID, serverUV); err != nil {
			return reset, err
		}
		f.UIDValidity = serverUV
	}
	return reset, nil
}

// folderDeepPassInterval is how often a folder gets a deep pass — the
// batched search + UID diff that heals gaps left by the initial window and
// re-baselines the expunge signal. It used to be every folder, every cycle, and
// that is the cost this issue is about: on an account that exposes a folder per
// label, with a very large All Mail, a sweep moved tens of thousands of UIDs per
// folder per cycle, the provider throttled it, and the sweep ran for minutes.
//
// Nothing about correctness rides on the interval: arrivals and deletions are
// both still noticed on the cycle they happen (see expungesLikely — the count
// catches a deletion even when an arrival masks it, because the arrivals are
// accounted for by name). What the deep pass adds is the static half — messages
// an earlier, narrower window skipped — and a periodic full re-baseline, so a
// mailbox that drifts out of step with its count cannot stay that way.
const folderDeepPassInterval = 30 * time.Minute

// deepDue reports whether this folder is owed a deep pass. It is a peek, not a
// claim: the pass is stamped by markDeep once it has actually run, so a pass cut
// short by a dropped connection is retried on the next cycle instead of being
// skipped for the whole interval.
func (a *account) deepDue(folderID int64) bool {
	t, ok := a.sweep.deep[folderID]
	return !ok || time.Since(t) >= folderDeepPassInterval
}

// markDeep records that a folder's deep pass has run. A folder the store has
// never seen gets one through firstFull, but it is stamped here too — the two
// paths must not both fire.
func (a *account) markDeep(folderID int64) {
	if a.sweep.deep == nil {
		a.sweep.deep = map[int64]time.Time{}
	}
	a.sweep.deep[folderID] = time.Now()
}

// baselineCount is the server's message count for a folder as of the last cycle
// that reconciled it (0 when nothing is known yet — see expungesLikely).
func (a *account) baselineCount(folderID int64) uint32 { return a.sweep.counts[folderID] }

func (a *account) setBaselineCount(folderID int64, n uint32) {
	if a.sweep.counts == nil {
		a.sweep.counts = map[int64]uint32{}
	}
	a.sweep.counts[folderID] = n
}

// expungesLikely reads the SELECT snapshot for the one question worth asking
// every cycle: did anything leave this mailbox? The store and the mailbox move
// together while the loop keeps up, so the server's count should equal what it
// held last cycle plus the arrivals this cycle fetched; a shortfall is a
// deletion, and only then is the O(mailbox) UID diff worth its cost.
//
// A count *above* baseline+arrivals is not a deletion: it is a mailbox that grew
// outside our window (see windowStartUID — messages mimux deliberately never
// stored), which the next deep pass re-baselines. A baseline of 0 means nothing
// is known yet — a folder still in its first cycle, which is a deep pass anyway.
func expungesLikely(baseline uint32, arrivals int, now uint32) bool {
	if baseline == 0 {
		return false
	}
	return int(baseline)+arrivals > int(now)
}

// syncFolder selects a folder and incrementally syncs new messages, flag
// changes (CONDSTORE) and expunged messages. It reports whether the folder's
// stored list actually changed (new/flag/expunge) so the caller can signal the
// browser to refresh — coalesced to one signal per cycle.
//
// The shape of a cycle is "cheap by default": the new-UID window and the
// CONDSTORE flag diff are proportional to what changed, and the two O(mailbox)
// passes — backfillWindow's SEARCH and reconcileExpunged's UID diff — run on a
// deep pass only. Everything a cycle skips, it either knows is unnecessary
// (nothing arrived, nothing can have left: see expungesLikely) or picks up on
// the next deep pass.
func (a *account) syncFolder(ctx context.Context, c *imapclient.Client, f *store.Folder, caps imap.CapSet) (changed bool, err error) {
	condstore := caps.Has(imap.CapCondStore)
	sel, err := c.Select(f.Name, &imap.SelectOptions{CondStore: condstore}).Wait()
	if err != nil {
		return false, err
	}
	reset, err := syncUIDValidity(a.m.st, f, sel.UIDValidity)
	if err != nil {
		return false, err
	}

	maxUID, err := a.m.st.MaxUID(f.ID)
	if err != nil {
		return false, err
	}
	firstFull := maxUID == 0 || reset
	if reset {
		// A UIDVALIDITY change renumbers the mailbox: any count we were holding
		// describes a folder that no longer exists, so drop the baseline and let
		// this pass (which is deep, firstFull) re-establish it.
		a.setBaselineCount(f.ID, 0)
	}
	deep := firstFull || a.deepDue(f.ID)

	// Window of new UIDs to fetch.
	var start imap.UID
	if firstFull {
		// "Sync history" setting (Settings → how far back to download): when set,
		// the date window is authoritative — SEARCH SINCE gives the oldest UID to
		// keep. Otherwise fall back to the message-count cap.
		if _, _, months, _ := a.syncSettings(); months > 0 {
			since := time.Now().AddDate(0, -months, 0)
			if data, err := c.UIDSearch(&imap.SearchCriteria{Since: since}, nil).Wait(); err == nil {
				if uids := data.AllUIDs(); len(uids) > 0 {
					start = minUID(uids)
				} else {
					start = sel.UIDNext // nothing in the window
				}
			}
		}
		if start == 0 { // no history limit, or the search failed: cap by count
			start = a.windowStartUID(c, sel.UIDNext)
		}
	} else {
		start = imap.UID(maxUID) + 1
	}

	// start:* (0 = "*"). announce is off for a folder's first full pass: that is
	// a backfill, not an arrival. It used to be enough that a first pass only
	// happened at startup, before LastSync was stamped — now ticking a folder in
	// Settings starts one on a long-running account, and a year of Archive must
	// not arrive as a year of notifications and webhook deliveries.
	due, err := a.m.st.DueStructures(f.ID, time.Now(), 10)
	if err != nil {
		return false, err
	}
	newCount, err := a.fetchSet(ctx, c, f, imap.UIDSet{{Start: start, Stop: 0}}, !firstFull)
	if err != nil {
		return false, err
	}
	changed = newCount > 0
	for _, uid := range due {
		if err := a.enrichStructure(f, uid, nil); err != nil {
			slog.Warn("message BODYSTRUCTURE retry failed", "account", a.cfg.Name, "folder", f.Name, "uid", uid, "err", err)
		} else {
			changed = true
		}
	}

	// Yield before the rest of the pass: whatever is queued — an interactive body
	// open, a server-side search — gets the connection here rather than after the
	// folder's reconciliation. This is the yield that matters most, because the
	// fetch above is the part that grows with new mail.
	served, err := a.yield(ctx, c)
	if err != nil {
		return changed, err
	}
	if served {
		// A command drained above SELECTs its own mailbox on this connection and
		// leaves it selected (TestSelectInboxAfterReadOnlyCommand) — an inbox body
		// open while the sweep works through Archive is exactly the interaction
		// this yield exists to serve. Everything below is UID work on f:
		// backfilling and reconciling against whatever mailbox is selected would
		// store another folder's mail as this one's, and delete this one's rows as
		// expunged. So put f back. Unconditionally, and without trusting
		// c.Mailbox() to have caught up with the drained SELECT yet (same test),
		// and without re-reading the selection: sel.NumMessages stays the count
		// this pass opened with, which setBaselineCount compares the next cycle's
		// arrivals against. One extra round trip, only on a pass that served a
		// command.
		if _, err := c.Select(f.Name, &imap.SelectOptions{CondStore: condstore}).Wait(); err != nil {
			return changed, err
		}
	}

	// Flag updates for existing messages (CONDSTORE only, cheap).
	if condstore && !firstFull && f.HighestModSeq > 0 {
		flagsChanged, err := a.fetchFlagChanges(c, f)
		if err != nil {
			return changed, err
		}
		changed = changed || flagsChanged
	}

	if deep {
		// Heal older gaps — e.g. left by a previous UIDNext-based window that
		// skipped older messages still present on the server (see
		// windowStartUID) — by fetching any of the newest server messages we
		// don't have yet. It costs sequence-window SEARCHes and a diff against every
		// stored UID, so it belongs on the deep pass, not on every cycle: what it
		// heals is static, and a gap that appears later is healed by the next one.
		// Used to be inbox-only because the inbox was all the steady state ever
		// re-read; now the caller decides which folders are worth a cycle, and
		// every one of them wants the same healing.
		if !firstFull {
			got, err := a.backfillWindow(ctx, c, f, sel.NumMessages, condstore)
			if err != nil {
				return changed, err
			}
			if got > 0 {
				changed = true
			}
		}

		// Reconcile expunged messages. A deep pass always does: on a folder's
		// first pass it costs nothing — everything stored was just fetched — and
		// it announces nothing; on a later one it is the periodic catch-up.
		expunged, err := a.reconcileExpunged(ctx, c, f, !firstFull, condstore)
		if err != nil {
			return changed, err
		}
		changed = changed || expunged
		a.markDeep(f.ID)
	} else if expungesLikely(a.baselineCount(f.ID), newCount, sel.NumMessages) {
		// Mail deleted (or moved away) in another client has to leave mimux too.
		// The count is the cheap way to hear about it without asking the server
		// for every UID — which is what this used to cost, in every folder, on
		// every cycle. When it says something left, this is the cycle to pay for
		// finding out which UIDs.
		expunged, err := a.reconcileExpunged(ctx, c, f, true, condstore)
		if err != nil {
			return changed, err
		}
		changed = changed || expunged
	}
	// Re-baseline from the server's own answer, whatever happened above —
	// including the case where the count moved for a reason this cycle could not
	// act on. The signal can never drift: the next cycle compares against what
	// the mailbox held at the end of this one.
	a.setBaselineCount(f.ID, sel.NumMessages)

	if condstore && sel.HighestModSeq > 0 {
		_ = a.m.st.SetFolderModSeq(f.ID, sel.HighestModSeq)
		f.HighestModSeq = sel.HighestModSeq
	}
	_ = a.m.st.RecountUnread(f.ID)

	return changed, nil
}

// windowStartUID returns the lowest UID to fetch so the newest
// MaxMessagesPerSync messages ACTUALLY present in the mailbox are covered. It
// SEARCHes for the real UIDs rather than using UIDNext-N: on Gmail, archived
// mail consumes UIDs without remaining in the mailbox, so UIDNext can be far
// larger than the message count, and UIDNext-N would skip older messages still
// present (e.g. old unread inbox mail).
func (a *account) windowStartUID(c *imapclient.Client, uidNext imap.UID) imap.UID {
	_, maxPerSync, _, _ := a.syncSettings()
	limit := uint32(maxPerSync) // #nosec G115 -- small positive admin-config value
	if limit == 0 {
		limit = 500
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil { // SEARCH failed: fall back to the UID-arithmetic window
		if uint32(uidNext) > limit {
			return uidNext - imap.UID(limit)
		}
		return 1
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return uidNext
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	if len(uids) > int(limit) {
		return uids[len(uids)-int(limit)]
	}
	return uids[0]
}

// deepBatchSize bounds the number of messages one deep-pass command asks the
// server to scan or return before the worker checks its interactive queue.
const deepBatchSize = 256

// yieldFolder serves queued commands and restores the pass's selected mailbox.
// A drained command may SELECT any folder, so every yield inside UID work needs
// this guard, even when the next command is only a SEARCH.
func (a *account) yieldFolder(ctx context.Context, c *imapclient.Client, f *store.Folder, condstore bool) error {
	served, err := a.yield(ctx, c)
	if err != nil || !served {
		return err
	}
	_, err = c.Select(f.Name, &imap.SelectOptions{CondStore: condstore}).Wait()
	return err
}

// backfillWindow fetches any of the newest MaxMessagesPerSync messages present
// on the server that aren't stored yet, healing gaps left by an older/narrower
// window without a manual re-sync. Search and fetch are batched so the worker
// can serve interactive commands during a deep pass.
func (a *account) backfillWindow(ctx context.Context, c *imapclient.Client, f *store.Folder, count uint32, condstore bool) (int, error) {
	_, maxPerSync, months, _ := a.syncSettings()
	limit := uint32(maxPerSync) // #nosec G115 -- small positive admin-config value
	if limit == 0 {
		limit = 500
	}
	crit := &imap.SearchCriteria{}
	if months > 0 {
		crit.Since = time.Now().AddDate(0, -months, 0)
	}
	// Search newest sequence windows first. UID gaps can be arbitrarily large on
	// Gmail, while sequence windows cap the number of messages in each command.
	// Only the newest limit matching UIDs are needed. The opening SELECT count is
	// a snapshot; arrivals during this pass belong to the next cycle.
	uids := make([]imap.UID, 0, limit)
	for end := count; end > 0 && len(uids) < int(limit); {
		start := uint32(1)
		if end > deepBatchSize {
			start = end - deepBatchSize + 1
		}
		crit.SeqNum = []imap.SeqSet{{{Start: start, Stop: end}}}
		data, err := c.UIDSearch(crit, nil).Wait()
		if err != nil {
			return 0, err
		}
		uids = append(uids, data.AllUIDs()...)
		if err := a.yieldFolder(ctx, c, f, condstore); err != nil {
			return 0, err
		}
		end = start - 1
	}
	if len(uids) == 0 {
		return 0, nil
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	if len(uids) > int(limit) {
		uids = uids[len(uids)-int(limit):]
	}
	existed, err := a.m.st.FolderUIDs(f.ID)
	if err != nil {
		return 0, err
	}
	var missing imap.UIDSet
	for _, u := range uids {
		if !existed[uint32(u)] {
			missing.AddNum(u)
		}
	}
	if len(missing) == 0 {
		return 0, nil
	}
	// Bound the body/envelope FETCH as well. UIDSet ranges can otherwise turn a
	// small command into thousands of fetched messages before the next yield.
	var fetched int
	for i := 0; i < len(uids); i += deepBatchSize {
		end := min(i+deepBatchSize, len(uids))
		var batch imap.UIDSet
		for _, uid := range uids[i:end] {
			if !existed[uint32(uid)] {
				batch.AddNum(uid)
			}
		}
		if len(batch) > 0 {
			n, err := a.fetchSet(ctx, c, f, batch, true)
			fetched += n
			if err != nil {
				return fetched, err
			}
		}
		if err := a.yieldFolder(ctx, c, f, condstore); err != nil {
			return fetched, err
		}
	}
	return fetched, nil
}

// fetchSet fetches envelope/flags/bodystructure + a snippet part for an explicit
// UID set and upserts each message. Returns the number of newly stored messages.
//
// announce is whether a newly stored message counts as an arrival worth telling
// anyone about (see signalNewMessage). Off for a folder's first full pass.
func (a *account) fetchSet(ctx context.Context, c *imapclient.Client, f *store.Folder, set imap.UIDSet, announce bool) (int, error) {
	opts := &imap.FetchOptions{
		UID:          true,
		Flags:        true,
		Envelope:     true,
		InternalDate: true,
		RFC822Size:   true,
		BodySection:  []*imap.FetchItemBodySection{snippetSection, refsHeaderSection},
	}
	msgs, err := c.Fetch(set, opts).Collect()
	if err != nil {
		return 0, err
	}
	// uid -> current stored labels; a UID absent is a message not yet stored.
	// Doubles as the "already have it" check FolderUIDs used to serve here.
	existed, err := a.m.st.FolderLabels(f.ID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, buf := range msgs {
		if buf.Envelope == nil {
			continue
		}
		prevLabels, isOld := existed[uint32(buf.UID)]
		isNew := !isOld
		if isNew {
			n++
		}
		m := messageFromBuffer(a.cfg.Name, f.ID, buf, prevLabels)
		// BODY[1] has no trustworthy transfer encoding without BODYSTRUCTURE.
		m.Snippet = ""
		m.StructureUnknown = true
		if err := a.m.st.UpsertMessage(m); err != nil {
			return n, err
		}
		if isNew {
			a.runRules(ctx, c, f, uint32(buf.UID), announce)
			if announce {
				// One hub broadcast, and every consumer (notifications, webhooks)
				// hangs off that. Non-blocking, so the sync loop pays nothing.
				a.signalNewMessage(f, buf)
			}
		}
	}
	// The decoder closes its connection on a malformed BODYSTRUCTURE. Use a
	// separate connection so the main metadata pass and later folders survive.
	var structureConn *imapclient.Client
	defer func() {
		if structureConn != nil {
			_ = structureConn.Close()
		}
	}()
	for _, buf := range msgs {
		if buf.Envelope == nil || !a.m.st.StructureRetryDue(f.ID, uint32(buf.UID), time.Now()) {
			continue
		}
		if a.cfg.IMAPHost == "" && a.connectFn == nil {
			continue
		}
		if structureConn == nil {
			structureConn, err = a.connect()
			if err == nil {
				_, err = structureConn.Select(f.Name, &imap.SelectOptions{ReadOnly: true}).Wait()
			}
			if err != nil {
				slog.Warn("message structure connection unavailable", "account", a.cfg.Name, "folder", f.Name, "uid", uint32(buf.UID), "err", err)
				_ = a.m.st.SetStructureRetry(f.ID, uint32(buf.UID), time.Now().Add(time.Hour))
				break
			}
		}
		if err := a.enrichStructureOn(structureConn, f, uint32(buf.UID), buf.FindBodySection(snippetSection)); err != nil {
			slog.Warn("message BODYSTRUCTURE unavailable", "account", a.cfg.Name, "folder", f.Name, "uid", uint32(buf.UID), "err", err)
			_ = structureConn.Close()
			structureConn = nil
		}
	}
	return n, nil
}

func (a *account) enrichStructure(f *store.Folder, uid uint32, snippet []byte) error {
	c, err := a.connect()
	if err != nil {
		_ = a.m.st.SetStructureRetry(f.ID, uid, time.Now().Add(time.Hour))
		return err
	}
	defer func() { _ = c.Close() }()
	if _, err = c.Select(f.Name, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		_ = a.m.st.SetStructureRetry(f.ID, uid, time.Now().Add(time.Hour))
		return err
	}
	return a.enrichStructureOn(c, f, uid, snippet)
}

func (a *account) enrichStructureOn(c *imapclient.Client, f *store.Folder, uid uint32, snippet []byte) error {
	set := imap.UIDSet{}
	set.AddNum(imap.UID(uid))
	data, err := c.Fetch(set, &imap.FetchOptions{UID: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true}, BodySection: []*imap.FetchItemBodySection{snippetSection}}).Collect()
	if err != nil {
		_ = a.m.st.SetStructureRetry(f.ID, uid, time.Now().Add(24*time.Hour))
		return err
	}
	if len(data) == 0 || data[0].BodyStructure == nil {
		_ = a.m.st.SetStructureRetry(f.ID, uid, time.Now().Add(24*time.Hour))
		return fmt.Errorf("UID %d has no BODYSTRUCTURE", uid)
	}
	if snippet == nil {
		snippet = data[0].FindBodySection(snippetSection)
	}
	preview := ""
	if snippet != nil {
		preview = partSnippet(snippet, data[0].BodyStructure)
	}
	return a.m.st.SetMessageStructure(f.ID, uid, hasAttachment(data[0].BodyStructure), preview)
}

// fetchFlagChanges pulls \Seen/\Flagged/keyword changes since the stored
// modseq and reports whether the server returned any changed messages (a
// proxy for "the list's read/star/label state changed", used to trigger a
// browser refresh).
//
// It is also where an EXTERNAL change is detected: each setter only touches a
// row whose value differs, so a write that moved a row is a change mimux did
// not make itself — every mimux mutation writes the store before pushing IMAP
// (see the invariant in actions.go), which makes the echo a no-op here. Each
// real one is announced as message-updated with origin "external".
//
// Limits, all structural to this function and documented for users on the
// webhooks docs page: CONDSTORE only (no CONDSTORE, no ChangedSince, no diff),
// and only for the folders a cycle actually syncs — the account's synced set in
// the steady state, everything else on reconnect. Labels are add-only, because MergeLabels
// is additive: a label another client removed cannot be seen from here.
func (a *account) fetchFlagChanges(c *imapclient.Client, f *store.Folder) (bool, error) {
	set := imap.UIDSet{{Start: 1, Stop: 0}}
	msgs, err := c.Fetch(set, &imap.FetchOptions{
		UID:          true,
		Flags:        true,
		ChangedSince: f.HighestModSeq,
	}).Collect()
	if err != nil || len(msgs) == 0 {
		return false, err
	}
	// One prefs read per folder per cycle, not one per message.
	announce := a.burstOK(len(msgs), "flag changes", f)
	for _, buf := range msgs {
		// MessageByFolderUID rather than the bare id lookup: merging keyword
		// flags into labels (below) needs the row's current stored value too.
		m, err := a.m.st.MessageByFolderUID(f.ID, uint32(buf.UID))
		if err != nil || m == nil {
			continue
		}
		// SetReadFromServer, not SetRead: a local mark-read whose \Seen push hasn't
		// landed yet must not be reverted to the server's stale "unread" here —
		// pushSeenDirty is still retrying it.
		read := hasFlag(buf.Flags, imap.FlagSeen)
		if moved, _ := a.m.st.SetReadFromServer(m.ID, read); moved && announce {
			a.m.updated(m, changeWord(read, "read", "unread"), originExternal)
		}
		starred := hasFlag(buf.Flags, imap.FlagFlagged)
		if moved, _ := a.m.st.SetStarred(m.ID, starred); moved && announce {
			a.m.updated(m, changeWord(starred, "starred", "unstarred"), originExternal)
		}
		if merged := MergeLabels(m.Labels, flagStrings(buf.Flags)); merged != m.Labels {
			if moved, _ := a.m.st.SetLabels(m.ID, merged); moved && announce {
				a.m.updated(m, "labeled", originExternal)
			}
		}
	}
	return len(msgs) > 0, nil
}

// reconcileExpunged deletes the folder's stored messages that the server no
// longer reports, and reports whether it removed any (which is what makes the
// open lists refresh, via the caller's signalListChanged).
//
// FETCH asks only for stored UIDs, in batches. Mail mimux never stored does
// not need to be transferred just to detect which stored messages vanished.
//
// announce carries the webhook event for each removal. It is off for a folder's
// first pass — everything stored was just fetched, so there is nothing to find,
// and a fresh install must not fire a delivery per message — and it is dropped
// for a batch over the external-change burst limit, which is a reconnect
// catching up rather than someone deleting four hundred messages by hand.
func (a *account) reconcileExpunged(ctx context.Context, c *imapclient.Client, f *store.Folder, announce, condstore bool) (bool, error) {
	stored, err := a.m.st.FolderUIDs(f.ID)
	if err != nil || len(stored) == 0 {
		return false, err
	}
	uids := make([]uint32, 0, len(stored))
	for uid := range stored {
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	var gone []uint32
	for i := 0; i < len(uids); i += deepBatchSize {
		end := min(i+deepBatchSize, len(uids))
		var batch imap.UIDSet
		for _, uid := range uids[i:end] {
			batch.AddNum(imap.UID(uid))
		}
		msgs, err := c.Fetch(batch, &imap.FetchOptions{UID: true}).Collect()
		if err != nil {
			return false, err
		}
		live := make(map[uint32]bool, len(msgs))
		for _, buf := range msgs {
			live[uint32(buf.UID)] = true
		}
		for _, uid := range uids[i:end] {
			if !live[uid] {
				gone = append(gone, uid)
			}
		}
		if err := a.yieldFolder(ctx, c, f, condstore); err != nil {
			return false, err
		}
	}
	if len(gone) == 0 {
		return false, nil
	}
	announce = announce && !a.getStatus().LastSync.IsZero() && a.burstOK(len(gone), "vanished messages", f)
	for _, uid := range gone {
		// Read before the delete: nothing downstream can describe a row that is
		// already gone, so the payload is built here or not at all.
		msg, _ := a.m.st.MessageByFolderUID(f.ID, uid)
		_ = a.m.st.DeleteMessageByUID(f.ID, uid)
		if announce && msg != nil {
			a.m.hub.broadcast(Event{Type: "message-deleted", Data: deletedData(msg, f)})
		}
	}
	return true, nil
}

// deletedData is the message-deleted event's payload, pre-serialised here
// because the row it describes no longer exists by the time anyone reads the
// event. Same summary shape as the message.received payload — never a body.
func deletedData(msg *store.Message, f *store.Folder) string {
	b, _ := json.Marshal(map[string]any{
		"id":         msg.ID,
		"account":    msg.Account,
		"folder":     f.Name,
		"folder_id":  f.ID,
		"from":       map[string]string{"name": msg.FromName, "address": msg.FromAddress},
		"subject":    msg.Subject,
		"date":       msg.Date.UTC().Format(time.RFC3339),
		"snippet":    msg.Snippet,
		"message_id": msg.MessageID,
	})
	return string(b)
}

// burstOK reports whether a batch of n externally-driven changes is small
// enough to be live activity worth announcing. Past the limit it is a reconnect
// catching up on an outage: the store still takes every write, and nothing is
// delivered. See Prefs.ExternalBurstLimit.
func (a *account) burstOK(n int, what string, f *store.Folder) bool {
	limit := a.m.st.GetPrefs().ExternalBurstLimit
	if limit <= 0 || n <= limit {
		return true
	}
	slog.Info("sync: burst over the external-change limit, not announcing",
		"account", a.cfg.Name, "folder", f.Name, "what", what, "count", n, "limit", limit)
	return false
}

func (a *account) messageID(folderID int64, uid uint32) (int64, error) {
	var id int64
	err := a.m.st.DB.QueryRow(`SELECT id FROM messages WHERE folder_id = ? AND uid = ?`, folderID, uid).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// snippetSection fetches the first ~2KB of body part 1 for the list preview.
var snippetSection = &imap.FetchItemBodySection{
	Part: []int{1}, Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: 2048},
}

// refsHeaderSection fetches only the References header (the envelope carries
// In-Reply-To but not References, which JWZ threading needs).
var refsHeaderSection = &imap.FetchItemBodySection{
	Specifier:    imap.PartSpecifierHeader,
	HeaderFields: []string{"References"},
	Peek:         true,
}

// parseRefsHeader extracts the message-id list from a raw "References: ..."
// header block, returning them space-joined and bare (bracket-stripped).
func parseRefsHeader(raw []byte) string {
	s := string(raw)
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[i+1:]
	}
	return strings.Join(splitMsgIDs(s), " ")
}

func messageFromBuffer(account string, folderID int64, buf *imapclient.FetchMessageBuffer, prevLabels string) *store.Message {
	env := buf.Envelope
	m := &store.Message{
		Account:       account,
		FolderID:      folderID,
		UID:           uint32(buf.UID),
		MessageID:     env.MessageID,
		InReplyTo:     strings.Join(env.InReplyTo, " "),
		Refs:          parseRefsHeader(buf.FindBodySection(refsHeaderSection)),
		Subject:       decodeHeader(env.Subject),
		Date:          env.Date,
		Size:          buf.RFC822Size,
		ToAddresses:   joinAddrs(env.To),
		CcAddresses:   joinAddrs(env.Cc),
		IsRead:        hasFlag(buf.Flags, imap.FlagSeen),
		IsStarred:     hasFlag(buf.Flags, imap.FlagFlagged),
		HasAttachment: hasAttachment(buf.BodyStructure),
		// Keyword flags (e.g. Triaged) merged onto whatever's already stored —
		// user-added labels included, since the server has no way to echo those
		// back (see Manager.SetLabel). Plain \Seen/\Answered/… flags are dropped
		// by MergeLabels' isNoiseLabel check, same predicate the label pills use.
		Labels: MergeLabels(prevLabels, flagStrings(buf.Flags)),
	}
	if m.Date.IsZero() {
		// Missing/unparseable Date: header — fall back to the server's
		// INTERNALDATE (when the message was received), never to now(), which
		// would stamp our sync time and float old mail to the top of the list.
		m.Date = buf.InternalDate
	}
	if len(env.From) > 0 {
		m.FromName = decodeHeader(env.From[0].Name)
		m.FromAddress = addrString(env.From[0])
	}
	// Snippet from the first body part.
	if snip := buf.FindBodySection(snippetSection); snip != nil && buf.BodyStructure != nil {
		m.Snippet = partSnippet(snip, buf.BodyStructure)
	}
	return m
}

// decodeHeader decodes RFC 2047 encoded-words left untouched by an IMAP
// server's ENVELOPE response. go-message's charset reader extends the standard
// decoder beyond UTF-8/ISO-8859-1 to common legacy charsets such as Windows-1252.
// A bad or unknown charset leaves the complete original value intact.
func decodeHeader(s string) string {
	d := mime.WordDecoder{CharsetReader: message.CharsetReader}
	decoded, err := d.DecodeHeader(s)
	if err != nil {
		return s
	}
	return decoded
}

// part1 returns the top-level body-structure node for IMAP part number 1
// (the part BODY[1] / snippetSection fetches).
func part1(bs imap.BodyStructure) imap.BodyStructure {
	switch b := bs.(type) {
	case *imap.BodyStructureSinglePart:
		return b
	case *imap.BodyStructureMultiPart:
		if len(b.Children) > 0 {
			return b.Children[0]
		}
	}
	return nil
}

// partSnippet renders the list preview for the fetched BODY[1] section. When
// part 1 is a leaf it is the raw body text; when part 1 is itself a multipart,
// BODY[1] returned the whole nested MIME entity (its headers and boundary
// markers), so we parse it to reach the real text instead of leaking MIME junk.
func partSnippet(raw []byte, bs imap.BodyStructure) string {
	if sp, ok := part1(bs).(*imap.BodyStructureSinglePart); ok {
		return snippetText(raw, sp.Encoding, sp.MediaType())
	}
	return nestedSnippet(raw)
}

// hasAttachment reports whether any part is an attachment or a named non-text file.
func hasAttachment(bs imap.BodyStructure) bool {
	if bs == nil {
		return false
	}
	found := false
	bs.Walk(func(_ []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		if isAttachmentPart(sp) {
			found = true
		}
		return true
	})
	return found
}

// minUID returns the smallest UID in the set (0 when empty).
func minUID(uids []imap.UID) imap.UID {
	var m imap.UID
	for _, u := range uids {
		if m == 0 || u < m {
			m = u
		}
	}
	return m
}

func hasFlag(flags []imap.Flag, want imap.Flag) bool {
	for _, fl := range flags {
		if fl == want {
			return true
		}
	}
	return false
}

// flagStrings converts a FETCH response's flags to plain strings for
// MergeLabels — imap.Flag is already the raw wire form (e.g. "\Seen",
// "Triaged"), just typed.
func flagStrings(flags []imap.Flag) []string {
	out := make([]string, len(flags))
	for i, fl := range flags {
		out[i] = string(fl)
	}
	return out
}

func joinAddrs(addrs []imap.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, addrString(a))
	}
	return strings.Join(parts, ", ")
}

func addrString(a imap.Address) string {
	if a.Mailbox == "" && a.Host == "" {
		return ""
	}
	return a.Mailbox + "@" + a.Host
}
