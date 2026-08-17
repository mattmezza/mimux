// SPDX-License-Identifier: AGPL-3.0-only
package search

import (
	"strconv"
	"strings"
)

// String renders a term back to its canonical query token, so a parsed query
// round-trips (used to rebuild the query when a UI pill is removed).
func (t Term) String() string {
	prefix := ""
	if t.Neg {
		prefix = "-"
	}
	switch t.Op {
	case "text":
		return quoteIfNeeded(t.Value, t.Phrase)
	case "before", "after":
		return prefix + t.Op + ":" + t.Date.Format("2006-01-02")
	case "larger", "smaller":
		return prefix + t.Op + ":" + strconv.FormatInt(t.Num, 10)
	default:
		return prefix + t.Op + ":" + quoteIfNeeded(t.Value, false)
	}
}

// Without returns the raw query with the term at index i removed.
func (q *SearchQuery) Without(i int) string {
	parts := make([]string, 0, len(q.Terms))
	for j, t := range q.Terms {
		if j == i {
			continue
		}
		parts = append(parts, t.String())
	}
	return strings.Join(parts, " ")
}

func quoteIfNeeded(v string, phrase bool) string {
	if phrase || strings.ContainsAny(v, " \t") {
		return `"` + v + `"`
	}
	return v
}
