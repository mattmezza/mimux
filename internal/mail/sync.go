package mail

import (
	"context"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/sm/internal/store"
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
// changes (CONDSTORE) and — on a full pass — expunged messages.
func (a *account) syncFolder(ctx context.Context, c *imapclient.Client, f *store.Folder, caps imap.CapSet) error {
	condstore := caps.Has(imap.CapCondStore)
	sel, err := c.Select(f.Name, &imap.SelectOptions{CondStore: condstore}).Wait()
	if err != nil {
		return err
	}
	reset, err := syncUIDValidity(a.m.st, f, sel.UIDValidity)
	if err != nil {
		return err
	}

	maxUID, err := a.m.st.MaxUID(f.ID)
	if err != nil {
		return err
	}
	firstFull := maxUID == 0 || reset

	// Window of new UIDs to fetch.
	var start imap.UID
	if firstFull {
		limit := uint32(a.m.cfg.Sync.MaxMessagesPerSync) // #nosec G115 -- small positive admin-config value
		if limit == 0 {
			limit = 500
		}
		if uint32(sel.UIDNext) > limit {
			start = sel.UIDNext - imap.UID(limit)
		} else {
			start = 1
		}
	} else {
		start = imap.UID(maxUID) + 1
	}

	newCount, err := a.fetchRange(ctx, c, f, start, condstore)
	if err != nil {
		return err
	}

	// Flag updates for existing messages (CONDSTORE only, cheap).
	if condstore && !firstFull && f.HighestModSeq > 0 {
		if err := a.fetchFlagChanges(c, f); err != nil {
			return err
		}
	}

	// Reconcile expunged messages on the full pass.
	if firstFull {
		if err := a.reconcileExpunged(c, f, start); err != nil {
			return err
		}
	}

	if condstore && sel.HighestModSeq > 0 {
		_ = a.m.st.SetFolderModSeq(f.ID, sel.HighestModSeq)
		f.HighestModSeq = sel.HighestModSeq
	}
	_ = a.m.st.RecountUnread(f.ID)

	if newCount > 0 && f.SpecialUse == "inbox" {
		a.m.hub.broadcast(Event{Type: "new-mail", Data: a.cfg.Name})
	}
	return nil
}

// fetchRange fetches envelope/flags/bodystructure + a snippet part for a UID
// range and upserts each message. Returns the number of newly stored messages.
func (a *account) fetchRange(ctx context.Context, c *imapclient.Client, f *store.Folder, start imap.UID, condstore bool) (int, error) {
	set := imap.UIDSet{{Start: start, Stop: 0}} // start:* (0 = "*")
	opts := &imap.FetchOptions{
		UID:           true,
		Flags:         true,
		Envelope:      true,
		RFC822Size:    true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection: []*imap.FetchItemBodySection{snippetSection, refsHeaderSection},
	}
	msgs, err := c.Fetch(set, opts).Collect()
	if err != nil {
		return 0, err
	}
	existed, err := a.m.st.FolderUIDs(f.ID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, buf := range msgs {
		if buf.Envelope == nil {
			continue
		}
		isNew := !existed[uint32(buf.UID)]
		if isNew {
			n++
		}
		if err := a.m.st.UpsertMessage(messageFromBuffer(a.cfg.Name, f.ID, buf)); err != nil {
			return n, err
		}
		if isNew {
			a.runRules(ctx, f.ID, uint32(buf.UID))
		}
	}
	_ = condstore
	return n, nil
}

// fetchFlagChanges pulls \Seen/\Flagged changes since the stored modseq.
func (a *account) fetchFlagChanges(c *imapclient.Client, f *store.Folder) error {
	set := imap.UIDSet{{Start: 1, Stop: 0}}
	msgs, err := c.Fetch(set, &imap.FetchOptions{
		UID:          true,
		Flags:        true,
		ChangedSince: f.HighestModSeq,
	}).Collect()
	if err != nil {
		return err
	}
	for _, buf := range msgs {
		id, err := a.messageID(f.ID, uint32(buf.UID))
		if err != nil || id == 0 {
			continue
		}
		_ = a.m.st.SetRead(id, hasFlag(buf.Flags, imap.FlagSeen))
		_ = a.m.st.SetStarred(id, hasFlag(buf.Flags, imap.FlagFlagged))
	}
	return nil
}

// reconcileExpunged deletes stored messages in the fetched window that the
// server no longer reports.
func (a *account) reconcileExpunged(c *imapclient.Client, f *store.Folder, start imap.UID) error {
	set := imap.UIDSet{{Start: start, Stop: 0}}
	msgs, err := c.Fetch(set, &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		return err
	}
	live := map[uint32]bool{}
	for _, buf := range msgs {
		live[uint32(buf.UID)] = true
	}
	stored, err := a.m.st.FolderUIDs(f.ID)
	if err != nil {
		return err
	}
	for uid := range stored {
		if uid >= uint32(start) && !live[uid] {
			_ = a.m.st.DeleteMessageByUID(f.ID, uid)
		}
	}
	return nil
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

func messageFromBuffer(account string, folderID int64, buf *imapclient.FetchMessageBuffer) *store.Message {
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
	}
	if m.Date.IsZero() {
		m.Date = time.Now()
	}
	if len(env.From) > 0 {
		m.FromName = env.From[0].Name
		m.FromAddress = addrString(env.From[0])
	}
	// Snippet from the first body part.
	if snip := buf.FindBodySection(snippetSection); snip != nil {
		enc, mt := part1Info(buf.BodyStructure)
		m.Snippet = snippetText(snip, enc, mt)
	}
	return m
}

func part1Info(bs imap.BodyStructure) (encoding, mediaType string) {
	switch b := bs.(type) {
	case *imap.BodyStructureSinglePart:
		return b.Encoding, b.MediaType()
	case *imap.BodyStructureMultiPart:
		if len(b.Children) > 0 {
			if sp, ok := b.Children[0].(*imap.BodyStructureSinglePart); ok {
				return sp.Encoding, sp.MediaType()
			}
			return "", b.Children[0].MediaType()
		}
	}
	return "", ""
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

func hasFlag(flags []imap.Flag, want imap.Flag) bool {
	for _, fl := range flags {
		if fl == want {
			return true
		}
	}
	return false
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
