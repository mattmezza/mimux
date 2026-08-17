// SPDX-License-Identifier: AGPL-3.0-only
// Package search parses mimux's query language into a SearchQuery and builds the
// local SQLite WHERE clause. IMAP-criteria mapping lives in internal/mail
// (it needs go-imap types); this package stays dependency-free so the parser
// and SQL builder are trivially testable.
package search

import (
	"strconv"
	"strings"
	"time"
)

// Term is one parsed clause. Every term ANDs with the others (no OR in the
// language). Neg negates it.
type Term struct {
	Op     string // from,to,cc,subject,body,is,has,in,label,before,after,larger,smaller,text
	Value  string // text value / keyword / folder / label
	Num    int64  // bytes for larger/smaller
	Date   time.Time
	Neg    bool
	Phrase bool // quoted exact phrase (text terms only)
}

// SearchQuery is a parsed query.
type SearchQuery struct {
	Terms []Term
	Raw   string
}

// TextTerms returns the free-text/subject/body values, for highlighting.
func (q *SearchQuery) TextTerms() []string {
	var out []string
	for _, t := range q.Terms {
		switch t.Op {
		case "text", "subject", "body":
			if !t.Neg && t.Value != "" {
				out = append(out, t.Value)
			}
		}
	}
	return out
}

// Empty reports whether the query has no terms.
func (q *SearchQuery) Empty() bool { return len(q.Terms) == 0 }

var valueOps = map[string]bool{
	"from": true, "to": true, "cc": true, "subject": true, "body": true,
	"in": true, "label": true, "before": true, "after": true,
	"larger": true, "smaller": true, "is": true, "has": true,
}

// Parse turns a raw query string into a SearchQuery.
func Parse(raw string) *SearchQuery {
	q := &SearchQuery{Raw: raw}
	for _, tok := range splitTokens(raw) {
		if t, ok := parseToken(tok); ok {
			q.Terms = append(q.Terms, t)
		}
	}
	return q
}

func parseToken(tok string) (Term, bool) {
	if tok == "" {
		return Term{}, false
	}
	neg := false
	body := tok
	// A leading '-' negates, but only when what follows is a real operator;
	// otherwise '-word' is just a bare word.
	if strings.HasPrefix(body, "-") && len(body) > 1 {
		rest := body[1:]
		if op, _, ok := splitOp(rest); ok && valueOps[op] {
			neg = true
			body = rest
		}
	}
	// A token that starts with a quote is always a phrase, never op:value.
	if !strings.HasPrefix(body, `"`) {
		if op, val, ok := splitOp(body); ok && valueOps[op] {
			if t, ok := buildOpTerm(op, val, neg); ok {
				return t, true
			}
			// Bad value (unparseable date/size, unknown keyword) degrades to a
			// bare word: the original token, quotes stripped.
		}
	}
	return Term{Op: "text", Value: unquote(tok), Phrase: isQuoted(tok)}, true
}

// splitOp splits "op:value" on the first colon.
func splitOp(s string) (op, val string, ok bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return strings.ToLower(s[:i]), s[i+1:], true
}

func buildOpTerm(op, val string, neg bool) (Term, bool) {
	switch op {
	case "from", "to", "cc", "subject", "body", "in", "label":
		v := unquote(val)
		if v == "" {
			return Term{}, false
		}
		return Term{Op: op, Value: v, Neg: neg, Phrase: op == "subject" || op == "body"}, true
	case "is":
		switch strings.ToLower(val) {
		case "unread", "read", "starred":
			return Term{Op: "is", Value: strings.ToLower(val), Neg: neg}, true
		}
		return Term{}, false
	case "has":
		switch strings.ToLower(val) {
		case "attachment", "link":
			return Term{Op: "has", Value: strings.ToLower(val), Neg: neg}, true
		}
		return Term{}, false
	case "before", "after":
		d, err := time.Parse("2006-01-02", val)
		if err != nil {
			return Term{}, false
		}
		return Term{Op: op, Date: d, Neg: neg}, true
	case "larger", "smaller":
		n, ok := parseSize(val)
		if !ok {
			return Term{}, false
		}
		return Term{Op: op, Num: n, Neg: neg}, true
	}
	return Term{}, false
}

// parseSize parses "5mb", "100kb", "512", "1.5gb" into bytes.
func parseSize(s string) (int64, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "gb"):
		mult, s = 1<<30, strings.TrimSuffix(s, "gb")
	case strings.HasSuffix(s, "mb"):
		mult, s = 1<<20, strings.TrimSuffix(s, "mb")
	case strings.HasSuffix(s, "kb"):
		mult, s = 1<<10, strings.TrimSuffix(s, "kb")
	case strings.HasSuffix(s, "b"):
		s = strings.TrimSuffix(s, "b")
	}
	if s == "" {
		return 0, false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f >= 0 {
		return int64(f * float64(mult)), true
	}
	return 0, false
}

// splitTokens splits on whitespace but keeps double-quoted spans (quotes
// retained) together, so `subject:"a b"` and `"a b"` stay one token.
func splitTokens(s string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case (r == ' ' || r == '\t' || r == '\n') && !inQuote:
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

func isQuoted(s string) bool {
	return len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`)
}

// unquote strips surrounding quotes and any inner quotes from a value.
func unquote(s string) string {
	if isQuoted(s) {
		s = s[1 : len(s)-1]
	}
	return strings.ReplaceAll(s, `"`, "")
}
