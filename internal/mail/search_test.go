package mail

import (
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/mattmezza/mimux/internal/search"
)

func TestCriteria(t *testing.T) {
	c := Criteria(search.Parse("from:alice subject:report is:unread larger:5mb after:2026-01-01"))

	if len(c.Header) != 2 {
		t.Fatalf("Header = %+v", c.Header)
	}
	if c.Header[0].Key != "FROM" || c.Header[0].Value != "alice" {
		t.Errorf("from header = %+v", c.Header[0])
	}
	if c.Header[1].Key != "SUBJECT" || c.Header[1].Value != "report" {
		t.Errorf("subject header = %+v", c.Header[1])
	}
	if len(c.NotFlag) != 1 || c.NotFlag[0] != imap.FlagSeen {
		t.Errorf("is:unread -> NotFlag = %+v", c.NotFlag)
	}
	if c.Larger != 5<<20 {
		t.Errorf("larger = %d", c.Larger)
	}
	if c.Since.IsZero() {
		t.Errorf("after -> Since not set")
	}
}

func TestCriteriaNegation(t *testing.T) {
	c := Criteria(search.Parse("-from:noreply"))
	if len(c.Not) != 1 {
		t.Fatalf("Not = %+v", c.Not)
	}
	if len(c.Not[0].Header) != 1 || c.Not[0].Header[0].Key != "FROM" || c.Not[0].Header[0].Value != "noreply" {
		t.Errorf("negated from = %+v", c.Not[0])
	}
}

func TestCriteriaTextAndBody(t *testing.T) {
	c := Criteria(search.Parse(`hello body:contract`))
	if len(c.Text) != 1 || c.Text[0] != "hello" {
		t.Errorf("Text = %+v", c.Text)
	}
	if len(c.Body) != 1 || c.Body[0] != "contract" {
		t.Errorf("Body = %+v", c.Body)
	}
}

func TestCriteriaSkipsUnportable(t *testing.T) {
	// has:attachment and in:archive have no portable IMAP key; they must not
	// produce spurious criteria.
	c := Criteria(search.Parse("has:attachment in:archive is:starred"))
	if len(c.Flag) != 1 || c.Flag[0] != imap.FlagFlagged {
		t.Errorf("Flag = %+v", c.Flag)
	}
	if len(c.Header) != 0 || len(c.Body) != 0 || len(c.Text) != 0 {
		t.Errorf("unportable ops leaked into criteria: %+v", c)
	}
}
