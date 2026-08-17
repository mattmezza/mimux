package mail

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mattmezza/mimux/internal/store"
)

// msg is a terse message builder for threading tests. seq drives Date order.
func msg(id, msgID, inReplyTo, refs, subject string, seq int) store.Message {
	return store.Message{
		MessageID: msgID,
		InReplyTo: inReplyTo,
		Refs:      refs,
		Subject:   subject,
		Date:      time.Date(2026, 1, 1, 0, 0, seq, 0, time.UTC),
		FromName:  id,
	}
}

// threadKeys returns, for each built thread, the sorted set of member subjects'
// FromName markers (we stash a stable marker in FromName) joined — a compact
// way to assert grouping regardless of thread order.
func groupSets(threads []Thread) []string {
	var out []string
	for _, t := range threads {
		var names []string
		for _, m := range t.Messages {
			names = append(names, m.FromName)
		}
		sort.Strings(names)
		out = append(out, strings.Join(names, ","))
	}
	sort.Strings(out)
	return out
}

func TestBuildThreads(t *testing.T) {
	tests := []struct {
		name string
		msgs []store.Message
		want []string // sorted member-marker sets, one per thread
	}{
		{
			name: "linear chain",
			msgs: []store.Message{
				msg("A", "a@x", "", "", "Hello", 1),
				msg("B", "b@x", "a@x", "<a@x>", "Re: Hello", 2),
				msg("C", "c@x", "b@x", "<a@x> <b@x>", "Re: Hello", 3),
			},
			want: []string{"A,B,C"},
		},
		{
			name: "missing intermediate promotes child",
			msgs: []store.Message{
				msg("A", "a@x", "", "", "Hello", 1),
				// B (b@x) is absent from the folder; C references A and B.
				msg("C", "c@x", "b@x", "<a@x> <b@x>", "Re: Hello", 3),
			},
			want: []string{"A,C"},
		},
		{
			name: "references win over in-reply-to on conflict",
			msgs: []store.Message{
				msg("A", "a@x", "", "", "Root", 1),
				msg("X", "x@x", "", "", "Other", 1),
				// In-Reply-To points at X, but References points at A. A wins.
				msg("B", "b@x", "x@x", "<a@x>", "Re: Root", 2),
			},
			want: []string{"A,B", "X"},
		},
		{
			name: "header-less same-subject messages stay apart (gmail parity)",
			msgs: []store.Message{
				msg("A", "a@x", "", "", "Project update", 1),
				msg("B", "b@x", "", "", "Re: Project update", 2),
				msg("C", "c@x", "", "", "Fwd: [list] Project update", 3),
				msg("D", "d@x", "", "", "Unrelated", 4),
			},
			want: []string{"A", "B", "C", "D"},
		},
		{
			name: "header-less different subjects stay apart",
			msgs: []store.Message{
				msg("A", "a@x", "", "", "One", 1),
				msg("B", "b@x", "", "", "Two", 2),
			},
			want: []string{"A", "B"},
		},
		{
			name: "reference loop does not hang",
			msgs: []store.Message{
				// a references b, b references a — a cycle.
				msg("A", "a@x", "b@x", "<b@x>", "Loop", 1),
				msg("B", "b@x", "a@x", "<a@x>", "Loop", 2),
			},
			want: []string{"A,B"},
		},
		{
			name: "gm_thrid overrides headers",
			msgs: []store.Message{
				// No usable headers, different subjects, but same thread id.
				withThrid(msg("A", "a@x", "", "", "Alpha", 1), "T1"),
				withThrid(msg("B", "b@x", "", "", "Beta", 2), "T1"),
				withThrid(msg("C", "c@x", "", "", "Gamma", 3), "T2"),
			},
			want: []string{"A,B", "C"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan []Thread, 1)
			go func() { done <- BuildThreads(tc.msgs) }()
			var got []Thread
			select {
			case got = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("BuildThreads hung (possible reference loop)")
			}
			if gs := groupSets(got); !equalStrings(gs, tc.want) {
				t.Errorf("grouping = %v, want %v", gs, tc.want)
			}
		})
	}
}

func TestThreadsSortedNewestFirst(t *testing.T) {
	threads := BuildThreads([]store.Message{
		msg("A", "a@x", "", "", "Old", 1),
		msg("B", "b@x", "", "", "New", 50),
	})
	if len(threads) != 2 {
		t.Fatalf("want 2 threads, got %d", len(threads))
	}
	if !threads[0].Latest.After(threads[1].Latest) {
		t.Errorf("threads not sorted newest-first")
	}
}

func TestThreadMessagesChronological(t *testing.T) {
	th := BuildThreads([]store.Message{
		msg("C", "c@x", "b@x", "<a@x> <b@x>", "Re: Hi", 3),
		msg("A", "a@x", "", "", "Hi", 1),
		msg("B", "b@x", "a@x", "<a@x>", "Re: Hi", 2),
	})
	if len(th) != 1 {
		t.Fatalf("want 1 thread, got %d", len(th))
	}
	got := th[0].Messages
	if got[0].FromName != "A" || got[len(got)-1].FromName != "C" {
		t.Errorf("messages not oldest-first: %s..%s", got[0].FromName, got[len(got)-1].FromName)
	}
	if !th[0].LatestMessage().Date.Equal(got[len(got)-1].Date) {
		t.Errorf("LatestMessage should be the last message")
	}
}

func withThrid(m store.Message, thrid string) store.Message {
	m.GmThrID = thrid
	return m
}

func withAccount(m store.Message, acct string) store.Message {
	m.Account = acct
	return m
}

// TestThreadingAccountScoping documents the unified-inbox threading policy:
// strong signals (a genuine References/Message-ID chain) MAY span accounts —
// that is the whole point of a unified conversation you're on via two mailboxes
// — but the weak signals must not. Per-mailbox Gmail thread ids are
// account-scoped, so colliding thread ids in two inboxes stay separate.
func TestThreadingAccountScoping(t *testing.T) {
	tests := []struct {
		name string
		msgs []store.Message
		want []string
	}{
		{
			// Duplicate Message-ID with no reference linkage (the same newsletter
			// delivered to two inboxes): the second copy gets its own container,
			// so the two stay apart.
			name: "duplicate newsletter across accounts stays separate",
			msgs: []store.Message{
				withAccount(msg("A", "news@x", "", "", "Weekly digest", 1), "acct1"),
				withAccount(msg("B", "news@x", "", "", "Weekly digest", 2), "acct2"),
			},
			want: []string{"A", "B"},
		},
		{
			// A real conversation where messages landed on different accounts
			// (distinct Message-IDs, proper reference chain): one thread.
			name: "genuine conversation spans accounts",
			msgs: []store.Message{
				withAccount(msg("A", "a@x", "", "", "Deploy", 1), "acct1"),
				withAccount(msg("B", "b@x", "a@x", "<a@x>", "Re: Deploy", 2), "acct2"),
				withAccount(msg("C", "c@x", "b@x", "<a@x> <b@x>", "Re: Deploy", 3), "acct1"),
			},
			want: []string{"A,B,C"},
		},
		{
			name: "same gm_thrid across accounts stays separate",
			msgs: []store.Message{
				withAccount(withThrid(msg("A", "a@x", "", "", "Alpha", 1), "T1"), "acct1"),
				withAccount(withThrid(msg("B", "b@x", "", "", "Beta", 2), "T1"), "acct2"),
			},
			want: []string{"A", "B"},
		},
		{
			name: "same account still threads normally",
			msgs: []store.Message{
				withAccount(msg("A", "a@x", "", "", "Hi", 1), "acct1"),
				withAccount(msg("B", "b@x", "a@x", "<a@x>", "Re: Hi", 2), "acct1"),
			},
			want: []string{"A,B"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if gs := groupSets(BuildThreads(tc.msgs)); !equalStrings(gs, tc.want) {
				t.Errorf("grouping = %v, want %v", gs, tc.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
