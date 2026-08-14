package mail

import (
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/sm/internal/config"
)

// TestSubmitTimesOut simulates a wedged worker (nothing ever drains a.cmds)
// and checks submit returns a real error within the caller's bound instead of
// hanging forever, and that the worker completing later doesn't block on the
// abandoned done channel.
func TestSubmitTimesOut(t *testing.T) {
	a := &account{cmds: make(chan cmd, 1), wake: make(chan struct{}, 1)}

	// A short-lived ctx stands in for submitTimeout so the test doesn't wait 30s.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := a.submit(ctx, func(*imapclient.Client) error { return nil })
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error from a submit whose worker never drains the queue")
	}
	if elapsed > time.Second {
		t.Fatalf("submit took %v, want bounded by ctx (~50ms)", elapsed)
	}

	// The command is still sitting in a.cmds. Simulate the worker finally
	// draining it after the caller gave up: the send on cm.done must not block
	// (buffered channel), proving the goroutine that ran fn wouldn't leak.
	select {
	case cm := <-a.cmds:
		done := make(chan struct{})
		go func() {
			cm.done <- nil // must not block even though submit already returned
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("late send on cm.done blocked: abandoned-caller leak")
		}
	default:
		t.Fatal("expected the queued command to still be sitting in a.cmds")
	}
}

// TestAnySyncing walks the overlap the "Syncing…" spinner exists for: two
// accounts, one finishing while the other is still going. The aggregate must
// stay true until the last one is done — and every transition must reach SSE
// subscribers, since the spinner is only ever told, never asks.
func TestAnySyncing(t *testing.T) {
	m := NewManager(&config.Config{}, nil)
	mk := func(name string) *account {
		a := &account{cfg: config.Account{Name: name}, m: m, status: AccountStatus{Account: name, State: "ok"}}
		m.accounts[name] = a
		return a
	}
	a, b := mk("A"), mk("B")
	events, unsubscribe := m.Subscribe()
	defer unsubscribe()

	steps := []struct {
		name string
		do   func()
		want bool
	}{
		{"idle", func() {}, false},
		{"A starts", func() { a.setStatus("syncing", "") }, true},
		{"B starts", func() { b.setStatus("syncing", "") }, true},
		{"A finishes, B still going", func() { a.setStatus("ok", "") }, true},
		{"B finishes", func() { b.setStatus("ok", "") }, false},
		{"an error is not a sync", func() { a.setStatus("error", "nope") }, false},
	}
	for _, s := range steps {
		s.do()
		if got := m.AnySyncing(); got != s.want {
			t.Errorf("%s: AnySyncing = %v, want %v", s.name, got, s.want)
		}
	}
	// One sync-status per setStatus above (the no-op first step aside).
	for i := 0; i < 5; i++ {
		select {
		case e := <-events:
			if e.Type != "sync-status" {
				t.Fatalf("event %d: type %q, want sync-status", i, e.Type)
			}
		default:
			t.Fatalf("only %d sync-status events broadcast, want 5: a transition nobody hears about can never reach the spinner", i)
		}
	}
}
