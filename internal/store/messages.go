package store

import (
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Message struct {
	ID            int64
	Account       string
	FolderID      int64
	UID           uint32
	MessageID     string
	InReplyTo     string
	Refs          string
	FromName      string
	FromAddress   string
	ToAddresses   string
	CcAddresses   string
	Subject       string
	Date          time.Time
	Size          int64
	IsRead        bool
	IsStarred     bool
	HasAttachment bool
	Snippet       string
	// GmThrID is Gmail's X-GM-THRID. NOTE: always "" today — nothing can
	// populate it. go-imap/v2 has no Gmail-extension support in any released
	// version (beta.8 is the newest tag; upstream master has none either), its
	// FETCH response parser hard-errors on unknown msg-att names
	// (imapclient/fetch.go: `unsupported msg-att name`), and it exports no raw
	// command escape hatch. thread.go already prefers this column, so threading
	// becomes Gmail-exact the moment the library ships X-GM-THRID.
	GmThrID string
	// Labels is the space-joined label set: Gmail's X-GM-LABELS when sync can
	// ever fetch them, plus the labels the user applies here (mail.AddLabel /
	// mail.RemoveLabel, which keep the "_ means space" token convention
	// mail.MessageLabels reverses on display). One column, one code path.
	Labels string
}

// UpsertMessage inserts a message or, on UID conflict, updates the mutable
// flag/metadata fields. date is re-asserted from the (deterministic) envelope
// so a re-fetch heals rows written before the INTERNALDATE fallback landed,
// where a missing Date: header had been now()-stamped and floated old mail to
// the top; for correct rows excluded.date equals the stored value (a no-op).
// labels only overwrites when the incoming value is non-empty: user-applied
// labels live in that same column and sync hands us "" for every message it
// can't read X-GM-LABELS for (today: all of them), which would wipe them.
func (s *Store) UpsertMessage(m *Message) error {
	_, err := s.DB.Exec(`
		INSERT INTO messages
			(account, folder_id, uid, message_id, in_reply_to, refs, from_name, from_address,
			 to_addresses, cc_addresses, subject, date, size, is_read, is_starred, has_attachment, snippet, gm_thrid, labels)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(folder_id, uid) DO UPDATE SET
			date = excluded.date,
			-- A local \Seen flip that hasn't reached the server yet outranks the
			-- server's flags: otherwise a re-fetch of this row (UIDVALIDITY reset,
			-- backfill) reverts a message the user already marked read.
			is_read = CASE WHEN seen_dirty = 1 THEN is_read ELSE excluded.is_read END,
			is_starred = excluded.is_starred,
			has_attachment = excluded.has_attachment, snippet = excluded.snippet,
			labels = CASE WHEN excluded.labels = '' THEN labels ELSE excluded.labels END`,
		m.Account, m.FolderID, m.UID, m.MessageID, m.InReplyTo, m.Refs, m.FromName, m.FromAddress,
		m.ToAddresses, m.CcAddresses, m.Subject, m.Date.UTC().Format(time.RFC3339), m.Size,
		b2i(m.IsRead), b2i(m.IsStarred), b2i(m.HasAttachment), m.Snippet, m.GmThrID, m.Labels)
	return err
}

func scanMessage(sc interface{ Scan(...any) error }) (*Message, error) {
	m := &Message{}
	var date string
	err := sc.Scan(&m.ID, &m.Account, &m.FolderID, &m.UID, &m.MessageID, &m.InReplyTo, &m.Refs,
		&m.FromName, &m.FromAddress, &m.ToAddresses, &m.CcAddresses, &m.Subject, &date, &m.Size,
		&m.IsRead, &m.IsStarred, &m.HasAttachment, &m.Snippet, &m.GmThrID, &m.Labels)
	if err != nil {
		return nil, err
	}
	m.Date, _ = time.Parse(time.RFC3339, date)
	return m, nil
}

const messageCols = `id, account, folder_id, uid, message_id, in_reply_to, refs, from_name,
	from_address, to_addresses, cc_addresses, subject, date, size, is_read, is_starred, has_attachment, snippet, gm_thrid, labels`

func (s *Store) MessageByID(id int64) (*Message, error) {
	m, err := scanMessage(s.DB.QueryRow(`SELECT `+messageCols+` FROM messages WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// ListMessages returns a folder's first page of messages, newest first: the
// newest limit conversations (whole), plus every conversation holding an unread
// message however old. Without the unread arm an unread message older than the
// limit-th newest is invisible to the list AND to BuildThreads, so it silently
// vanishes from the Unread filter and its thread renders short.
func (s *Store) ListMessages(folderID int64, limit int) ([]Message, error) {
	msgs, _, err := s.ListMessagesPage(folderID, "", limit)
	return msgs, err
}

// ListMessagesPage is ListMessages one page at a time — see listPage.
func (s *Store) ListMessagesPage(folderID int64, before string, limit int) ([]Message, string, error) {
	return s.listPage(`m.folder_id = ?`, []any{folderID}, before, limit)
}

// ListUnifiedInbox returns the first page of every account's inbox folder,
// newest first — the unified "All inboxes" view. Same rule as ListMessages, so
// the list can never show fewer unread threads than the tab badge counts.
func (s *Store) ListUnifiedInbox(limit int) ([]Message, error) {
	msgs, _, err := s.ListUnifiedInboxPage("", limit)
	return msgs, err
}

// ListUnifiedInboxPage is ListUnifiedInbox one page at a time — see listPage.
func (s *Store) ListUnifiedInboxPage(before string, limit int) ([]Message, string, error) {
	return s.listPage(`f.special_use = 'inbox'`, nil, before, limit)
}

// listPage returns one page of a list scope, newest first, paged by
// CONVERSATION rather than by row: a page holds every message of the
// conversations whose newest message falls in its band, so a page boundary can
// never cut a thread in half — LIMIT/OFFSET on date would hand BuildThreads the
// younger half of a conversation on one page and the older half on the next,
// rendering it as two half-rows.
//
// before is the opaque cursor the previous page returned ("" starts at the top);
// next is "" once the scope is exhausted, which is what makes the list's
// load-more sentinel disappear. It is a (date, id) pair rather than an offset
// because the list mutates under the user constantly — a sync inserts at the
// top, archive/delete removes in the middle — and an offset skips and
// duplicates rows across exactly that churn.
//
// NOTE: one union-find pass over the scope's threading columns per page
// fetch — the same scan ConversationSizes already runs per render. Persist a
// thread id column if the store ever outgrows that. A page is limit
// CONVERSATIONS, so it is only as bounded as they are: one pathological
// conversation (a mailing list where every message References the same root)
// pulls all of its members into one page. Cap the members per conversation to
// the newest few if that ever shows up.
func (s *Store) listPage(where string, args []any, before string, limit int) ([]Message, string, error) {
	rows, err := s.conversationRows(where, args...)
	if err != nil {
		return nil, "", err
	}
	// Fold the rows into conversations: member ids, the newest member (what the
	// list sorts and pages on) and whether anything in there is unread.
	type conv struct {
		key    string // "<date>_<id>" of the newest member: sort key and cursor
		unread bool
		ids    []int64
	}
	byConv := map[string]*conv{}
	order := make([]*conv, 0, len(rows))
	for _, r := range rows {
		c := byConv[r.Conv]
		if c == nil {
			c = &conv{}
			byConv[r.Conv] = c
			order = append(order, c)
		}
		c.ids = append(c.ids, r.ID)
		c.unread = c.unread || !r.Read
		// Dates are stored as UTC RFC3339 (fixed width), so they sort
		// lexicographically and the id merely breaks ties: the whole cursor is a
		// plain string compare, in Go and in the URL alike.
		if k := r.Date + "_" + strconv.FormatInt(r.ID, 10); k > c.key {
			c.key = k
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i].key > order[j].key })

	var ids []int64
	next, n, more := "", 0, false
	for _, c := range order {
		if before != "" && (c.key >= before || c.unread) {
			continue // already delivered: newer than the cursor, or unread on page one
		}
		if n < limit {
			ids = append(ids, c.ids...)
			next = c.key
			n++
			continue
		}
		if before == "" && c.unread {
			// Page one keeps every unread conversation, however old — the window
			// rule ListMessages has always promised. Keep sweeping for more of
			// them rather than breaking out here.
			ids = append(ids, c.ids...)
			continue
		}
		more = true
		if before != "" {
			break // later pages have no unread tail to sweep up
		}
	}
	if !more { // nothing left below this page: the sentinel must go away
		next = ""
	}
	if len(ids) == 0 {
		return nil, "", nil
	}
	in := make([]string, len(ids))
	for i, id := range ids {
		in[i] = strconv.FormatInt(id, 10)
	}
	// #nosec G202 -- the interpolated ids are int64 primary keys just read out of
	// this same table, never user input.
	msgRows, err := s.DB.Query(`SELECT ` + messageCols + ` FROM messages
		WHERE id IN (` + strings.Join(in, ",") + `) ORDER BY date DESC`)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = msgRows.Close() }()
	var out []Message
	for msgRows.Next() {
		m, err := scanMessage(msgRows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, *m)
	}
	return out, next, msgRows.Err()
}

// convRow is one message row's threading facts — enough to group a scope into
// conversations without loading whole messages.
type convRow struct {
	ID      int64
	Account string
	Date    string // stored UTC RFC3339, so it sorts as a plain string
	Read    bool
	Key     string // this row's own grouping key: its Message-ID, or a row token
	Conv    string // the conversation it landed in (its union-find root)
}

// conversationRows groups the messages matching where (a fragment over
// `messages m JOIN folders f`) into conversations with a union-find over
// Message-ID/References: the same closure ThreadMessages walks, but once for
// the whole scope instead of once per row. Coarser than BuildThreads' JWZ pass
// and deliberately so — a page cut on these groups can never split a thread the
// finer grouping would keep together.
//
// The join is what drops rows orphaned by a deleted folder (e.g. an account
// renamed in config.toml): they are invisible to the reading pane, so they must
// stay invisible to the list and to the size count too.
func (s *Store) conversationRows(where string, args ...any) ([]convRow, error) {
	// #nosec G202 -- where is a fixed fragment from this package; every value is bound.
	rows, err := s.DB.Query(`SELECT m.id, m.account, m.date, m.is_read, m.message_id, m.refs, m.in_reply_to
		FROM messages m JOIN folders f ON f.id = m.folder_id WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	parent := map[string]string{}
	var find func(string) string
	find = func(k string) string {
		p, ok := parent[k]
		if !ok {
			parent[k] = k
			return k
		}
		if p == k {
			return k
		}
		r := find(p)
		parent[k] = r
		return r
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	var out []convRow
	for rows.Next() {
		var r convRow
		var msgID, refs, inReplyTo string
		if err := rows.Scan(&r.ID, &r.Account, &r.Date, &r.Read, &msgID, &refs, &inReplyTo); err != nil {
			return nil, err
		}
		// Grouping runs on the bare Message-ID (ThreadMessages' closure is
		// deliberately not account-scoped); callers scope the members they count.
		r.Key = msgID
		if msgID == "" { // nothing to dedup or link on: stands alone, like BuildThreads
			r.Key = "\x00row\x00" + strconv.FormatInt(r.ID, 10)
		}
		find(r.Key)
		refIDs := splitIDs(refs)
		if len(refIDs) == 0 {
			refIDs = splitIDs(inReplyTo)
		}
		for _, rid := range refIDs {
			union(r.Key, rid)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Roots only settle once every union is in.
	for i := range out {
		out[i].Conv = find(out[i].Key)
	}
	return out, nil
}

// ThreadMessages returns every message of seed's conversation, across ALL
// folders, found by Message-ID/References closure rather than by scanning a
// list window: the reading pane must show the Sent replies and the
// read-and-old members that ListMessages/ListUnifiedInbox never return.
//
// Deliberately NOT account-scoped: a genuine reference chain may span accounts
// in the unified view (mail.TestThreadingAccountScoping, and the /t render
// test both assert it). BuildThreads applies the account policy for the weak
// signals — the dedup key here is (account, message_id) so the same newsletter
// delivered to two accounts stays two rows.
//
// Gmail's IMAP publishes one message as separate rows in INBOX, [Gmail]/All
// Mail and [Gmail]/Important, so those copies are deduped, keeping the
// inbox/sent one when there is one (that row's folder+UID is the sensible
// target for replies and flag changes).
func (s *Store) ThreadMessages(seed *Message) ([]Message, error) {
	ids := map[string]bool{}
	var pending []string
	add := func(raw string) {
		for _, id := range splitIDs(raw) {
			if !ids[id] {
				ids[id] = true
				pending = append(pending, id)
			}
		}
	}
	add(seed.MessageID)
	add(seed.Refs)
	add(seed.InReplyTo)

	best := map[string]Message{} // message_id -> chosen row
	var out []Message
	// NOTE: bounded at 5 rounds — converges in 1-2 for real mail; a
	// pathological reference chain just renders a slightly short thread.
	for round := 0; round < 5 && len(pending) > 0; round++ {
		batch := pending
		pending = nil
		where := make([]string, 0, len(batch))
		var args []any
		for _, id := range batch {
			// Token match on the space-joined refs/in-reply-to columns (no LIKE,
			// so `_` and `%` inside a Message-ID can't wildcard).
			where = append(where, `(m.message_id = ? OR instr(' '||m.refs||' ', ' '||?||' ') > 0
				OR instr(' '||m.in_reply_to||' ', ' '||?||' ') > 0)`)
			args = append(args, id, id, id)
		}
		// #nosec G202 -- the concatenated fragment is a fixed template repeated
		// per id; every Message-ID travels as a bound arg, never as SQL text.
		rows, err := s.DB.Query(`SELECT `+messageCols+` FROM (
			SELECT m.*, CASE WHEN f.special_use IN ('inbox', 'sent') THEN 0 ELSE 1 END AS pri
			FROM messages m JOIN folders f ON f.id = m.folder_id
			WHERE `+strings.Join(where, " OR ")+`
		) ORDER BY pri, id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			m, err := scanMessage(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			if m.MessageID == "" { // no id to dedup on: keep every copy, as BuildThreads does
				out = append(out, *m)
				continue
			}
			key := m.Account + "\x00" + m.MessageID
			if _, dup := best[key]; dup {
				continue // ORDER BY pri already put the inbox/sent copy first
			}
			best[key] = *m
			out = append(out, *m)
			add(m.MessageID)
			add(m.Refs)
			add(m.InReplyTo)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}

// ConversationSizes returns, for every stored message row, how many messages
// its whole conversation holds across ALL folders — what gmail.com shows on a
// list row. The list itself is inbox-scoped and windowed, so a Thread built
// from it only counts its inbox members (a Sent reply is invisible to it).
//
// One conversationRows pass over the whole store — the same grouping the list's
// paging runs, once for every row instead of once per visible row (~150 queries
// a render). Sizes count distinct (account, message_id) pairs, so Gmail's
// INBOX/All Mail/Important copies of one message collapse the way
// ThreadMessages dedups them, while the same newsletter in two accounts stays
// two messages.
//
// NOTE: full table scan per list render — trivial at a few thousand rows
// (five small columns, one join). Cache it keyed on the sync generation, or
// persist a thread id column, if the store ever grows past that.
func (s *Store) ConversationSizes() (map[int64]int, error) {
	rows, err := s.conversationRows(`1 = 1`)
	if err != nil {
		return nil, err
	}
	size := map[string]int{}
	counted := map[string]bool{} // account\x00grouping-key: one message, however many copies
	for _, r := range rows {
		mk := r.Account + "\x00" + r.Key
		if counted[mk] {
			continue
		}
		counted[mk] = true
		size[r.Conv]++
	}
	out := make(map[int64]int, len(rows))
	for _, r := range rows {
		out[r.ID] = size[r.Conv]
	}
	return out, nil
}

// splitIDs splits a Message-ID / References / In-Reply-To value into bare ids.
func splitIDs(s string) []string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if id := strings.Trim(f, "<> \t"); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// SetLabels stores the space-joined label set for a message.
func (s *Store) SetLabels(id int64, labels string) error {
	_, err := s.DB.Exec(`UPDATE messages SET labels = ? WHERE id = ?`, labels, id)
	return err
}

// DistinctLabels returns every distinct non-empty labels column value — the
// raw space-joined rows, since splitting them into individual labels is
// mail.AllLabels' job (store can't import mail). Feeds the add-label
// autocomplete. NOTE: one DISTINCT over a short column, no index; add one
// if the message table ever gets big enough for it to show.
func (s *Store) DistinctLabels() ([]string, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT labels FROM messages WHERE labels != ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// FolderUIDs returns the set of UIDs currently stored for a folder, for
// reconciling against the server (detecting expunged messages).
func (s *Store) FolderUIDs(folderID int64) (map[uint32]bool, error) {
	rows, err := s.DB.Query(`SELECT uid FROM messages WHERE folder_id = ?`, folderID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[uint32]bool{}
	for rows.Next() {
		var u uint32
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out[u] = true
	}
	return out, rows.Err()
}

// FolderLabels returns every stored message's label value in a folder, keyed
// by UID — sync's cheap way (one query for the whole folder, like FolderUIDs)
// to look up what to merge newly-fetched flags into without a query per
// message. A UID absent from the result is a message not yet stored.
func (s *Store) FolderLabels(folderID int64) (map[uint32]string, error) {
	rows, err := s.DB.Query(`SELECT uid, labels FROM messages WHERE folder_id = ?`, folderID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[uint32]string{}
	for rows.Next() {
		var u uint32
		var l string
		if err := rows.Scan(&u, &l); err != nil {
			return nil, err
		}
		out[u] = l
	}
	return out, rows.Err()
}

// MaxUID returns the highest stored UID in a folder, 0 if empty.
func (s *Store) MaxUID(folderID int64) (uint32, error) {
	var u sql.NullInt64
	if err := s.DB.QueryRow(`SELECT MAX(uid) FROM messages WHERE folder_id = ?`, folderID).Scan(&u); err != nil {
		return 0, err
	}
	return uint32(u.Int64), nil // #nosec G115 -- UID fits uint32 by protocol
}

// SetRead records a LOCAL read/unread decision: it flips is_read and marks the
// \Seen push as still owed (seen_dirty). Every local entry point goes through
// here — the read handler, mark-read-at-open, filter rules — so the intent
// survives a failed/timed-out IMAP push and a restart, and sync can't overwrite
// it in the meantime. mail.Manager.SetRead clears the marker once the flag is
// on the server; the account worker retries it every cycle until then.
func (s *Store) SetRead(id int64, read bool) error {
	_, err := s.DB.Exec(`UPDATE messages SET is_read = ?, seen_dirty = 1 WHERE id = ?`, b2i(read), id)
	return err
}

// SetReadFromServer applies the server's \Seen truth during sync. It skips rows
// whose own \Seen push is still owed — those are ahead of the server, not behind
// it, and overwriting them is exactly how a mark-read "came back unread".
func (s *Store) SetReadFromServer(id int64, read bool) error {
	_, err := s.DB.Exec(`UPDATE messages SET is_read = ? WHERE id = ? AND seen_dirty = 0`, b2i(read), id)
	return err
}

// ClearSeenDirty marks a message's \Seen state as confirmed on the server. The
// is_read guard makes it a no-op when the user has since toggled again — that
// newer state is still owed a push of its own.
func (s *Store) ClearSeenDirty(id int64, read bool) error {
	_, err := s.DB.Exec(`UPDATE messages SET seen_dirty = 0 WHERE id = ? AND is_read = ?`, id, b2i(read))
	return err
}

// DirtySeen returns an account's messages whose \Seen push is still owed,
// oldest first, for the worker to retry.
func (s *Store) DirtySeen(account string, limit int) ([]Message, error) {
	rows, err := s.DB.Query(`SELECT `+messageCols+` FROM messages
		WHERE account = ? AND seen_dirty = 1 ORDER BY id LIMIT ?`, account, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *Store) SetStarred(id int64, starred bool) error {
	_, err := s.DB.Exec(`UPDATE messages SET is_starred = ? WHERE id = ?`, b2i(starred), id)
	return err
}

// DeleteMessageByUID removes a single message row (used when the server
// reports it moved/expunged).
func (s *Store) DeleteMessageByUID(folderID int64, uid uint32) error {
	_, err := s.DB.Exec(`DELETE FROM messages WHERE folder_id = ? AND uid = ?`, folderID, uid)
	return err
}

func (s *Store) DeleteMessage(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM messages WHERE id = ?`, id)
	return err
}

// SetMessageFolder relocates a message's local row to another folder, used for
// the optimistic move/undo-move flow (the real IMAP move is deferred; see
// Server.schedulePendingMove).
func (s *Store) SetMessageFolder(id, folderID int64) error {
	_, err := s.DB.Exec(`UPDATE messages SET folder_id = ? WHERE id = ?`, folderID, id)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
