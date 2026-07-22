package mail

import (
	"slices"
	"strings"
	"testing"
	"time"

	emmail "github.com/emersion/go-message/mail"

	"github.com/mattmezza/sm/internal/config"
)

func TestPrefixSubject(t *testing.T) {
	cases := []struct {
		kind, in, want string
	}{
		{"reply", "Hello", "Re: Hello"},
		{"reply", "Re: Hello", "Re: Hello"}, // no stacking
		{"reply", "re: hello", "re: hello"}, // already prefixed, case-insensitive
		{"reply_all", "Hello", "Re: Hello"},
		{"forward", "Hello", "Fwd: Hello"},
		{"forward", "Fwd: Hello", "Fwd: Hello"},
		{"new", "Hello", "Hello"},
	}
	for _, c := range cases {
		if got := PrefixSubject(c.kind, c.in); got != c.want {
			t.Errorf("PrefixSubject(%q, %q) = %q, want %q", c.kind, c.in, got, c.want)
		}
	}
}

func TestQuoteBody(t *testing.T) {
	date := time.Date(2026, 7, 20, 10, 30, 0, 0, time.UTC)
	got := QuoteBody(date, "Alice <alice@example.com>", "line one\nline two")
	want := "On Mon, 20 Jul 2026 10:30, Alice <alice@example.com> wrote:\n> line one\n> line two\n"
	if got != want {
		t.Errorf("QuoteBody =\n%q\nwant\n%q", got, want)
	}
}

func TestReplyRecipients(t *testing.T) {
	got := ReplyRecipients("me@example.com", "Alice <alice@example.com>")
	want := []string{"Alice <alice@example.com>"}
	if !slices.Equal(got, want) {
		t.Errorf("ReplyRecipients = %v, want %v", got, want)
	}
	// Replying to yourself (e.g. a message you sent) drops out entirely.
	if got := ReplyRecipients("me@example.com", "me@example.com"); len(got) != 0 {
		t.Errorf("ReplyRecipients(self) = %v, want empty", got)
	}
}

func TestReplyAllRecipients(t *testing.T) {
	cases := []struct {
		name           string
		self           string
		from, to, cc   []string
		wantTo, wantCc []string
	}{
		{
			name: "basic",
			self: "me@example.com",
			from: []string{"alice@example.com"}, to: []string{"me@example.com", "bob@example.com"}, cc: []string{"carol@example.com"},
			wantTo: []string{"alice@example.com", "bob@example.com"}, wantCc: []string{"carol@example.com"},
		},
		{
			name: "dedup between to and cc",
			self: "me@example.com",
			from: []string{"alice@example.com"}, to: []string{"me@example.com"}, cc: []string{"alice@example.com", "dave@example.com"},
			wantTo: []string{"alice@example.com"}, wantCc: []string{"dave@example.com"},
		},
		{
			name: "self in from is dropped",
			self: "me@example.com",
			from: []string{"me@example.com"}, to: []string{"bob@example.com"}, cc: nil,
			wantTo: []string{"bob@example.com"}, wantCc: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			to, cc := ReplyAllRecipients(c.self, c.from, c.to, c.cc)
			if !slices.Equal(to, c.wantTo) {
				t.Errorf("to = %v, want %v", to, c.wantTo)
			}
			if !slices.Equal(cc, c.wantCc) {
				t.Errorf("cc = %v, want %v", cc, c.wantCc)
			}
		})
	}
}

func TestComputeReferences(t *testing.T) {
	cases := []struct {
		name, origRefs, origMessageID, want string
	}{
		{"no prior refs", "", "abc@example.com", "<abc@example.com>"},
		{"appends to existing chain", "<a@x> <b@x>", "c@x", "<a@x> <b@x> <c@x>"},
		{"dedup", "<a@x> <c@x>", "c@x", "<a@x> <c@x>"},
		{"no message id", "<a@x>", "", "<a@x>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ComputeReferences(c.origRefs, c.origMessageID); got != c.want {
				t.Errorf("ComputeReferences(%q, %q) = %q, want %q", c.origRefs, c.origMessageID, got, c.want)
			}
		})
	}
}

func TestSplitAddrList(t *testing.T) {
	got := SplitAddrList(" a@x , b@x ,, ")
	want := []string{"a@x", "b@x"}
	if !slices.Equal(got, want) {
		t.Errorf("SplitAddrList = %v, want %v", got, want)
	}
	if got := SplitAddrList("  "); got != nil {
		t.Errorf("SplitAddrList(blank) = %v, want nil", got)
	}
}

// TestBuildMessage builds a message then parses it back with go-message,
// asserting the headers/threading/subject came through correctly.
func TestBuildMessage(t *testing.T) {
	cfg := config.Account{Name: "Work", Email: "me@example.com"}
	in := ComposeInput{
		To:         []string{"Alice <alice@example.com>"},
		Cc:         []string{"bob@example.com"},
		Bcc:        []string{"hidden@example.com"},
		Subject:    "Re: Project status",
		Body:       "Sounds good.\nSee you then.",
		InReplyTo:  "orig@example.com",
		References: "<root@example.com> <orig@example.com>",
	}
	now := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)

	raw, msgID, err := BuildMessage(cfg, in, now)
	if err != nil {
		t.Fatal(err)
	}
	if msgID == "" {
		t.Fatal("expected a generated Message-ID")
	}
	if !strings.HasSuffix(msgID, "@example.com") {
		t.Errorf("Message-ID host = %q, want it to use the account's domain", msgID)
	}

	r, err := emmail.CreateReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse built message: %v", err)
	}
	from, _ := r.Header.AddressList("From")
	if len(from) != 1 || from[0].Address != "me@example.com" || from[0].Name != "Work" {
		t.Errorf("From = %+v", from)
	}
	to, _ := r.Header.AddressList("To")
	if len(to) != 1 || to[0].Address != "alice@example.com" {
		t.Errorf("To = %+v", to)
	}
	cc, _ := r.Header.AddressList("Cc")
	if len(cc) != 1 || cc[0].Address != "bob@example.com" {
		t.Errorf("Cc = %+v", cc)
	}
	// Bcc must never appear in the header.
	if v := r.Header.Get("Bcc"); v != "" {
		t.Errorf("Bcc leaked into header: %q", v)
	}
	if subj, _ := r.Header.Subject(); subj != "Re: Project status" {
		t.Errorf("Subject = %q", subj)
	}
	if irt, _ := r.Header.MsgIDList("In-Reply-To"); len(irt) != 1 || irt[0] != "orig@example.com" {
		t.Errorf("In-Reply-To = %v", irt)
	}
	if refs, _ := r.Header.MsgIDList("References"); len(refs) != 2 || refs[1] != "orig@example.com" {
		t.Errorf("References = %v", refs)
	}
	if got, _ := r.Header.MessageID(); got != msgID {
		t.Errorf("parsed Message-ID = %q, want %q", got, msgID)
	}
}
