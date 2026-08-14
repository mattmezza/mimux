package store

import (
	"strings"
	"time"
)

// AddressSuggestion is one compose-typeahead candidate: an address, the display
// name last seen with it, and the rank score that ordered it.
type AddressSuggestion struct {
	Address string
	Name    string
	Score   int
}

// Display renders the suggestion the way compose fields want it back. A name
// carrying the punctuation that delimits an address list is dropped rather than
// quoted: the whole app splits those lists on bare commas (SplitAddrList), so a
// "Surname, Name" display name would be torn in half on send.
func (a AddressSuggestion) Display() string {
	if a.Name == "" || strings.ContainsAny(a.Name, `,<>"`) {
		return a.Address
	}
	return a.Name + " <" + a.Address + ">"
}

// AddressSuggestMinLen is the shortest fragment worth querying: the pool scan is
// a leading-wildcard LIKE (no index can serve it), and one character matches
// most of the mailbox for no useful narrowing.
const AddressSuggestMinLen = 2

// suggestAddressesSQL ranks correspondents by frequency × recency.
//
// Two pools, because there is no contacts table: senders of mail we hold, and
// the recipients of our own Sent copies — the latter split out of the
// comma-joined to/cc columns by the recursive CTE (SQLite has no split()).
// People we wrote to weigh 4× a sender, and anything recent weighs 3× older
// mail, so real correspondents beat high-volume newsletters.
//
// ponytail: the sender half is a full scan of messages (~50ms at 13k rows).
// An index can't help `LIKE '%x%'`; if the mailbox grows an order of magnitude,
// switch the pre-filter to the existing messages_fts `from_addr`/`to_addr`
// columns with a prefix MATCH.
const suggestAddressesSQL = `
WITH RECURSIVE
sent(rest, date) AS (
    SELECT m.to_addresses || ',' || m.cc_addresses || ',', m.date
      FROM messages m JOIN folders f ON f.id = m.folder_id
     WHERE f.special_use = 'sent'
       AND (m.to_addresses LIKE ?1 ESCAPE '\' OR m.cc_addresses LIKE ?1 ESCAPE '\')
    UNION ALL
    SELECT substr(rest, instr(rest, ',') + 1), date FROM sent WHERE instr(rest, ',') > 0
),
pool(addr, name, date, w) AS (
    SELECT lower(from_address), from_name, date, 1 FROM messages
     WHERE from_address <> ''
       AND (from_address LIKE ?1 ESCAPE '\' OR from_name LIKE ?1 ESCAPE '\')
       AND folder_id NOT IN (SELECT id FROM folders WHERE special_use IN ('spam', 'trash'))
    UNION ALL
    SELECT lower(trim(substr(rest, 1, instr(rest, ',') - 1))), '', date, 4
      FROM sent
     WHERE instr(rest, ',') > 0
       AND trim(substr(rest, 1, instr(rest, ',') - 1)) LIKE ?1 ESCAPE '\'
)
SELECT p.addr,
       COALESCE((SELECT n.name FROM pool n
                  WHERE n.addr = p.addr AND n.name <> '' ORDER BY n.date DESC LIMIT 1), ''),
       SUM(p.w * CASE WHEN p.date >= ?2 THEN 3 ELSE 1 END) AS score
  FROM pool p
 WHERE p.addr <> '' AND p.addr LIKE '%@%'
 GROUP BY p.addr
 ORDER BY score DESC, MAX(p.date) DESC
 LIMIT ?3`

// suggestRecentWindow is how far back mail still counts as "recent" for the
// score boost.
const suggestRecentWindow = 180 * 24 * time.Hour

// SuggestAddresses returns compose-typeahead candidates matching q anywhere in
// the address or display name, most-corresponded-with first, deduped
// case-insensitively. `exclude` (the user's own identities, which are never
// useful suggestions) is matched case-insensitively.
func (s *Store) SuggestAddresses(q string, exclude []string, limit int) ([]AddressSuggestion, error) {
	q = strings.TrimSpace(q)
	if len(q) < AddressSuggestMinLen {
		return nil, nil
	}
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		if e != "" {
			skip[strings.ToLower(e)] = true
		}
	}
	// Over-fetch so dropping our own identities can't leave a short list.
	recent := time.Now().UTC().Add(-suggestRecentWindow).Format(time.RFC3339)
	rows, err := s.DB.Query(suggestAddressesSQL, "%"+likeEscape(q)+"%", recent, limit+len(skip))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AddressSuggestion
	for rows.Next() {
		var a AddressSuggestion
		if err := rows.Scan(&a.Address, &a.Name, &a.Score); err != nil {
			return nil, err
		}
		if skip[a.Address] {
			continue
		}
		out = append(out, a)
		if len(out) == limit {
			break
		}
	}
	return out, rows.Err()
}

// likeEscape neutralises the LIKE wildcards a user can type, so "a_b" matches
// literally instead of any character.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
