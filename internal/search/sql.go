package search

import (
	"strings"
	"time"
)

// Scope bounds a local search.
type Scope string

const (
	ScopeFolder  Scope = "folder"
	ScopeAccount Scope = "account"
	ScopeAll     Scope = "all"
)

// ParseScope normalizes an incoming scope string, defaulting to account.
func ParseScope(s string) Scope {
	switch Scope(s) {
	case ScopeFolder:
		return ScopeFolder
	case ScopeAll:
		return ScopeAll
	default:
		return ScopeAccount
	}
}

// BuildLocalSQL builds the WHERE fragment (without the leading "WHERE") and its
// args for a local message search. Free-text/subject/body positive terms go
// through the messages_fts MATCH subquery; everything else is a plain column
// predicate. Pure string building — no DB needed, so it's unit-testable.
func BuildLocalSQL(q *SearchQuery, scope Scope, account string, folderID int64) (where string, args []any) {
	var conds []string
	var fts []string

	add := func(c string, a ...any) {
		conds = append(conds, c)
		args = append(args, a...)
	}
	like := func(v string) string { return "%" + v + "%" }

	for _, t := range q.Terms {
		switch t.Op {
		case "text": // bare words are never negated in this language (negation is operator-only)
			fts = append(fts, ftsToken(t.Value))
		case "subject":
			if t.Neg {
				add("subject NOT LIKE ?", like(t.Value))
			} else {
				fts = append(fts, "subject:"+ftsToken(t.Value))
			}
		case "body": // NOTE: body: is server-only; locally we match the snippet as best effort.
			if t.Neg {
				add("snippet NOT LIKE ?", like(t.Value))
			} else {
				fts = append(fts, "snippet:"+ftsToken(t.Value))
			}
		case "from":
			c := "(from_address LIKE ? OR from_name LIKE ?)"
			add(neg(t.Neg, c), like(t.Value), like(t.Value))
		case "to":
			add(negLike(t.Neg, "to_addresses"), like(t.Value))
		case "cc":
			add(negLike(t.Neg, "cc_addresses"), like(t.Value))
		case "label":
			add(negLike(t.Neg, "labels"), like(t.Value))
		case "is":
			add(isCond(t.Value, t.Neg))
		case "has":
			switch t.Value {
			case "attachment":
				add(boolCond("has_attachment", t.Neg))
			case "link": // NOTE: link detection is a snippet substring scan, good enough for the list preview.
				add(negLike(t.Neg, "snippet"), like("http"))
			}
		case "in":
			c := "folder_id IN (SELECT id FROM folders WHERE lower(special_use) = ? OR name LIKE ?)"
			if t.Neg {
				c = "folder_id NOT IN (SELECT id FROM folders WHERE lower(special_use) = ? OR name LIKE ?)"
			}
			add(c, strings.ToLower(t.Value), like(t.Value))
		case "larger":
			if t.Neg {
				add("size <= ?", t.Num)
			} else {
				add("size > ?", t.Num)
			}
		case "smaller":
			if t.Neg {
				add("size >= ?", t.Num)
			} else {
				add("size < ?", t.Num)
			}
		case "before":
			if t.Neg {
				add("date >= ?", dayStart(t.Date))
			} else {
				add("date < ?", dayStart(t.Date))
			}
		case "after":
			if t.Neg {
				add("date < ?", dayStart(t.Date))
			} else {
				add("date >= ?", dayStart(t.Date))
			}
		}
	}

	if len(fts) > 0 {
		add("messages.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)", strings.Join(fts, " "))
	}

	switch scope {
	case ScopeFolder:
		add("folder_id = ?", folderID)
	case ScopeAccount:
		add("account = ?", account)
	}

	if len(conds) == 0 {
		return "1=1", nil
	}
	return strings.Join(conds, " AND "), args
}

func neg(n bool, cond string) string {
	if n {
		return "NOT " + cond
	}
	return cond
}

func negLike(n bool, col string) string {
	if n {
		return col + " NOT LIKE ?"
	}
	return col + " LIKE ?"
}

func boolCond(col string, n bool) string {
	if n {
		return col + " = 0"
	}
	return col + " = 1"
}

func isCond(v string, n bool) string {
	switch v {
	case "unread":
		return boolCond("is_read", !n)
	case "read":
		return boolCond("is_read", n)
	case "starred":
		return boolCond("is_starred", n)
	}
	return "1=1"
}

func dayStart(d time.Time) string {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
}

func ftsToken(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}
