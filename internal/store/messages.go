package store

import (
	"database/sql"
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
	// GmThrID is Gmail's X-GM-THRID. ponytail: always "" today — nothing can
	// populate it. go-imap/v2 has no Gmail-extension support in any released
	// version (beta.8 is the newest tag; upstream master has none either), its
	// FETCH response parser hard-errors on unknown msg-att names
	// (imapclient/fetch.go: `unsupported msg-att name`), and it exports no raw
	// command escape hatch. thread.go already prefers this column, so threading
	// becomes Gmail-exact the moment the library ships X-GM-THRID.
	GmThrID string
	Labels  string // space-joined Gmail labels, "" for non-Gmail
}

// UpsertMessage inserts a message or, on UID conflict, updates the mutable
// flag/metadata fields. date is re-asserted from the (deterministic) envelope
// so a re-fetch heals rows written before the INTERNALDATE fallback landed,
// where a missing Date: header had been now()-stamped and floated old mail to
// the top; for correct rows excluded.date equals the stored value (a no-op).
func (s *Store) UpsertMessage(m *Message) error {
	_, err := s.DB.Exec(`
		INSERT INTO messages
			(account, folder_id, uid, message_id, in_reply_to, refs, from_name, from_address,
			 to_addresses, cc_addresses, subject, date, size, is_read, is_starred, has_attachment, snippet, gm_thrid, labels)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(folder_id, uid) DO UPDATE SET
			date = excluded.date, is_read = excluded.is_read, is_starred = excluded.is_starred,
			has_attachment = excluded.has_attachment, snippet = excluded.snippet, labels = excluded.labels`,
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

// ListMessages returns a folder's messages newest first: the newest limit rows,
// plus every unread row however old. Without the unread arm an unread message
// older than the limit-th newest is invisible to the list AND to BuildThreads,
// so it silently vanishes from the Unread filter and its thread renders short.
// ponytail: no pagination — a *read* thread member older than the window is
// still missing from the reading pane. Add real paging (or fetch a thread by
// its id) when that bites.
func (s *Store) ListMessages(folderID int64, limit int) ([]Message, error) {
	rows, err := s.DB.Query(`SELECT `+messageCols+` FROM messages
		WHERE folder_id = ? AND (is_read = 0 OR id IN (
			SELECT id FROM messages WHERE folder_id = ? ORDER BY date DESC LIMIT ?))
		ORDER BY date DESC`, folderID, folderID, limit)
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

// ListUnifiedInbox returns messages from every account's inbox folder, newest
// first — the unified "All inboxes" view. Same window rule as ListMessages:
// newest limit rows plus every unread row, so the list can never show fewer
// unread threads than the tab badge counts.
func (s *Store) ListUnifiedInbox(limit int) ([]Message, error) {
	const inboxes = `folder_id IN (SELECT id FROM folders WHERE special_use = 'inbox')`
	rows, err := s.DB.Query(`SELECT `+messageCols+` FROM messages
		WHERE `+inboxes+` AND (is_read = 0 OR id IN (
			SELECT id FROM messages WHERE `+inboxes+` ORDER BY date DESC LIMIT ?))
		ORDER BY date DESC`, limit)
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
	// ponytail: bounded at 5 rounds — converges in 1-2 for real mail; a
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
// One projection of the threading columns for every row, then a union-find over
// Message-ID/References in Go: the same closure ThreadMessages walks, but once
// for the whole store instead of once per visible row (~150 queries a render).
// Sizes count distinct (account, message_id) pairs, so Gmail's INBOX/All
// Mail/Important copies of one message collapse the way ThreadMessages
// dedups them, while the same newsletter in two accounts stays two messages.
//
// ponytail: full table scan per list render — trivial at a few thousand rows
// (five small columns, no joins). Cache it keyed on the sync generation, or
// persist a thread id column, if the store ever grows past that.
func (s *Store) ConversationSizes() (map[int64]int, error) {
	// JOIN folders like ThreadMessages does: rows orphaned by a deleted folder
	// (e.g. an account renamed in config.toml) are invisible to the reading pane,
	// so they must not inflate the list row's count either.
	rows, err := s.DB.Query(`SELECT m.id, m.account, m.message_id, m.refs, m.in_reply_to
		FROM messages m JOIN folders f ON f.id = m.folder_id`)
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

	// Grouping runs on the bare Message-ID (ThreadMessages' closure is
	// deliberately not account-scoped); the counted member is (account, id).
	rowKey := map[int64]string{}
	members := map[string]string{} // account\x00message_id -> its grouping key
	for rows.Next() {
		var id int64
		var account, msgID, refs, inReplyTo string
		if err := rows.Scan(&id, &account, &msgID, &refs, &inReplyTo); err != nil {
			return nil, err
		}
		key := msgID
		if msgID == "" { // nothing to dedup or link on: stands alone, like BuildThreads
			key = "\x00row\x00" + strconv.FormatInt(id, 10)
		}
		find(key)
		members[account+"\x00"+key] = key
		rowKey[id] = key
		refIDs := splitIDs(refs)
		if len(refIDs) == 0 {
			refIDs = splitIDs(inReplyTo)
		}
		for _, rid := range refIDs {
			union(key, rid)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	size := map[string]int{}
	for _, k := range members {
		size[find(k)]++
	}
	out := make(map[int64]int, len(rowKey))
	for id, k := range rowKey {
		out[id] = size[find(k)]
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

// SetLabels stores the space-joined Gmail label set for a message.
func (s *Store) SetLabels(id int64, labels string) error {
	_, err := s.DB.Exec(`UPDATE messages SET labels = ? WHERE id = ?`, labels, id)
	return err
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

// MaxUID returns the highest stored UID in a folder, 0 if empty.
func (s *Store) MaxUID(folderID int64) (uint32, error) {
	var u sql.NullInt64
	if err := s.DB.QueryRow(`SELECT MAX(uid) FROM messages WHERE folder_id = ?`, folderID).Scan(&u); err != nil {
		return 0, err
	}
	return uint32(u.Int64), nil // #nosec G115 -- UID fits uint32 by protocol
}

func (s *Store) SetRead(id int64, read bool) error {
	_, err := s.DB.Exec(`UPDATE messages SET is_read = ? WHERE id = ?`, b2i(read), id)
	return err
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
