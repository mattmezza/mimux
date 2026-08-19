// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

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

// syncFolder selects a folder and incrementally syncs new messages, flag
// changes (CONDSTORE) and — on a full pass — expunged messages. It reports
// whether the folder's stored list actually changed (new/flag/expunge) so the
// caller can signal the browser to refresh — coalesced to one signal per cycle.
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

	newCount, err := a.fetchRange(ctx, c, f, start, condstore)
	if err != nil {
		return false, err
	}
	changed = newCount > 0

	// Flag updates for existing messages (CONDSTORE only, cheap).
	if condstore && !firstFull && f.HighestModSeq > 0 {
		flagsChanged, err := a.fetchFlagChanges(c, f)
		if err != nil {
			return changed, err
		}
		changed = changed || flagsChanged
	}

	// Heal older gaps in the inbox — e.g. left by a previous UIDNext-based window
	// that skipped older messages still present on the server (see windowStartUID)
	// — by fetching any of the newest server messages we don't have yet. Scoped to
	// the inbox: it's an extra SEARCH per cycle, the unread badge is inbox-only,
	// and once caught up the diff is empty. NOTE: inbox-only; widen if other
	// folders show stale counts.
	if f.SpecialUse == "inbox" && !firstFull {
		if got, _ := a.backfillWindow(ctx, c, f); got > 0 {
			changed = true
		}
	}

	// Reconcile expunged messages on the full pass.
	if firstFull {
		expunged, err := a.reconcileExpunged(c, f, start)
		if err != nil {
			return changed, err
		}
		changed = changed || expunged
	}

	if condstore && sel.HighestModSeq > 0 {
		_ = a.m.st.SetFolderModSeq(f.ID, sel.HighestModSeq)
		f.HighestModSeq = sel.HighestModSeq
	}
	_ = a.m.st.RecountUnread(f.ID)

	return changed, nil
}

// fetchRange fetches envelope/flags/bodystructure + a snippet part for a UID
// range and upserts each message. Returns the number of newly stored messages.
func (a *account) fetchRange(ctx context.Context, c *imapclient.Client, f *store.Folder, start imap.UID, condstore bool) (int, error) {
	_ = condstore
	return a.fetchSet(ctx, c, f, imap.UIDSet{{Start: start, Stop: 0}}) // start:* (0 = "*")
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

// backfillWindow fetches any of the newest MaxMessagesPerSync messages present
// on the server that aren't stored yet, healing gaps left by an older/narrower
// window without a manual re-sync. Cheap once caught up (a SEARCH + empty diff).
func (a *account) backfillWindow(ctx context.Context, c *imapclient.Client, f *store.Folder) (int, error) {
	_, maxPerSync, months, _ := a.syncSettings()
	limit := uint32(maxPerSync) // #nosec G115 -- small positive admin-config value
	if limit == 0 {
		limit = 500
	}
	crit := &imap.SearchCriteria{}
	if months > 0 {
		crit.Since = time.Now().AddDate(0, -months, 0)
	}
	data, err := c.UIDSearch(crit, nil).Wait()
	if err != nil {
		return 0, err
	}
	uids := data.AllUIDs()
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
	return a.fetchSet(ctx, c, f, missing)
}

// fetchSet fetches envelope/flags/bodystructure + a snippet part for an explicit
// UID set and upserts each message. Returns the number of newly stored messages.
func (a *account) fetchSet(ctx context.Context, c *imapclient.Client, f *store.Folder, set imap.UIDSet) (int, error) {
	opts := &imap.FetchOptions{
		UID:           true,
		Flags:         true,
		Envelope:      true,
		InternalDate:  true,
		RFC822Size:    true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*imap.FetchItemBodySection{snippetSection, refsHeaderSection},
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
		if err := a.m.st.UpsertMessage(messageFromBuffer(a.cfg.Name, f.ID, buf, prevLabels)); err != nil {
			return n, err
		}
		if isNew {
			a.runRules(ctx, c, f.ID, uint32(buf.UID))
			// One hub broadcast, and every consumer (notifications, webhooks)
			// hangs off that. Non-blocking, so the sync loop pays nothing.
			a.signalNewMessage(f, buf)
		}
	}
	return n, nil
}

// fetchFlagChanges pulls \Seen/\Flagged/keyword changes since the stored
// modseq and reports whether the server returned any changed messages (a
// proxy for "the list's read/star/label state changed", used to trigger a
// browser refresh).
func (a *account) fetchFlagChanges(c *imapclient.Client, f *store.Folder) (bool, error) {
	set := imap.UIDSet{{Start: 1, Stop: 0}}
	msgs, err := c.Fetch(set, &imap.FetchOptions{
		UID:          true,
		Flags:        true,
		ChangedSince: f.HighestModSeq,
	}).Collect()
	if err != nil {
		return false, err
	}
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
		_, _ = a.m.st.SetReadFromServer(m.ID, hasFlag(buf.Flags, imap.FlagSeen))
		_, _ = a.m.st.SetStarred(m.ID, hasFlag(buf.Flags, imap.FlagFlagged))
		if merged := MergeLabels(m.Labels, flagStrings(buf.Flags)); merged != m.Labels {
			_, _ = a.m.st.SetLabels(m.ID, merged)
		}
	}
	return len(msgs) > 0, nil
}

// reconcileExpunged deletes stored messages in the fetched window that the
// server no longer reports, reporting whether it removed any.
func (a *account) reconcileExpunged(c *imapclient.Client, f *store.Folder, start imap.UID) (bool, error) {
	set := imap.UIDSet{{Start: start, Stop: 0}}
	msgs, err := c.Fetch(set, &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		return false, err
	}
	live := map[uint32]bool{}
	for _, buf := range msgs {
		live[uint32(buf.UID)] = true
	}
	stored, err := a.m.st.FolderUIDs(f.ID)
	if err != nil {
		return false, err
	}
	removed := false
	for uid := range stored {
		if uid >= uint32(start) && !live[uid] {
			_ = a.m.st.DeleteMessageByUID(f.ID, uid)
			removed = true
		}
	}
	return removed, nil
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
		Subject:       env.Subject,
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
		m.FromName = env.From[0].Name
		m.FromAddress = addrString(env.From[0])
	}
	// Snippet from the first body part.
	if snip := buf.FindBodySection(snippetSection); snip != nil {
		m.Snippet = partSnippet(snip, buf.BodyStructure)
	}
	return m
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
		if d := sp.Disposition(); d != nil && strings.EqualFold(d.Value, "attachment") {
			found = true
		}
		if sp.Filename() != "" && !strings.HasPrefix(sp.MediaType(), "text/") {
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
