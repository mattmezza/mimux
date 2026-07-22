package search

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Term
	}{
		{"empty", "", nil},
		{"bare words AND", "hello world", []Term{
			{Op: "text", Value: "hello"}, {Op: "text", Value: "world"},
		}},
		{"from and subject", "from:alice@x.com subject:report", []Term{
			{Op: "from", Value: "alice@x.com"},
			{Op: "subject", Value: "report", Phrase: true},
		}},
		{"case-insensitive operator", "FROM:bob", []Term{{Op: "from", Value: "bob"}}},
		{"negation", "-from:noreply@x.com", []Term{
			{Op: "from", Value: "noreply@x.com", Neg: true},
		}},
		{"quoted phrase", `"quarterly report"`, []Term{
			{Op: "text", Value: "quarterly report", Phrase: true},
		}},
		{"quoted phrase with colon", `subject:"time: now"`, []Term{
			{Op: "subject", Value: "time: now", Phrase: true},
		}},
		{"bare phrase with colon stays text", `"foo:bar baz"`, []Term{
			{Op: "text", Value: "foo:bar baz", Phrase: true},
		}},
		{"is and has", "is:unread has:attachment", []Term{
			{Op: "is", Value: "unread"}, {Op: "has", Value: "attachment"},
		}},
		{"has link", "has:link", []Term{{Op: "has", Value: "link"}}},
		{"negated is", "-is:read", []Term{{Op: "is", Value: "read", Neg: true}}},
		{"in and label", "in:archive label:work", []Term{
			{Op: "in", Value: "archive"}, {Op: "label", Value: "work"},
		}},
		{"sizes", "larger:5mb smaller:100kb", []Term{
			{Op: "larger", Num: 5 << 20}, {Op: "smaller", Num: 100 << 10},
		}},
		{"size plain bytes and gb", "larger:512 smaller:1gb", []Term{
			{Op: "larger", Num: 512}, {Op: "smaller", Num: 1 << 30},
		}},
		{"bad size degrades to word", "larger:huge", []Term{
			{Op: "text", Value: "larger:huge"},
		}},
		{"dates", "after:2026-01-01 before:2026-02-01", []Term{
			{Op: "after", Date: mustDate("2026-01-01")},
			{Op: "before", Date: mustDate("2026-02-01")},
		}},
		{"bad date degrades to word", "before:2026-13-99", []Term{
			{Op: "text", Value: "before:2026-13-99"},
		}},
		{"unknown operator degrades to word", "foo:bar", []Term{
			{Op: "text", Value: "foo:bar"},
		}},
		{"unknown is keyword degrades", "is:banana", []Term{
			{Op: "text", Value: "is:banana"},
		}},
		{"negated unknown keeps dash word", "-hello", []Term{
			{Op: "text", Value: "-hello"},
		}},
		{"mixed", `from:alice "big deal" -subject:spam larger:2mb`, []Term{
			{Op: "from", Value: "alice"},
			{Op: "text", Value: "big deal", Phrase: true},
			{Op: "subject", Value: "spam", Neg: true, Phrase: true},
			{Op: "larger", Num: 2 << 20},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.in).Terms
			if len(got) != len(tc.want) {
				t.Fatalf("got %d terms %+v, want %d %+v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if !termEq(got[i], tc.want[i]) {
					t.Errorf("term %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func termEq(a, b Term) bool {
	return a.Op == b.Op && a.Value == b.Value && a.Num == b.Num &&
		a.Neg == b.Neg && a.Phrase == b.Phrase && a.Date.Equal(b.Date)
}

func mustDate(s string) time.Time {
	d, _ := time.Parse("2006-01-02", s)
	return d
}

func TestTextTerms(t *testing.T) {
	q := Parse(`hello subject:report -subject:spam from:alice body:contract`)
	got := q.TextTerms()
	want := map[string]bool{"hello": true, "report": true, "contract": true}
	if len(got) != len(want) {
		t.Fatalf("TextTerms = %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected highlight term %q", g)
		}
	}
}
