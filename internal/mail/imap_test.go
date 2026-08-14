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

// newTestAccount builds a worker with the channels Reload would have given it,
// registered on m so Status/AnySyncing see it.
func newTestAccount(m *Manager, name, state string) *account {
	a := &account{
		cfg:    config.Account{Name: name},
		m:      m,
		cmds:   make(chan cmd, 4),
		wake:   make(chan struct{}, 1),
		nudge:  make(chan struct{}, 1),
		status: AccountStatus{Account: name, State: state},
	}
	m.accounts[name] = a
	return a
}

// TestReadOnlyCommandSkipsSync is requirement (1): a command that cannot change
// the mailbox must get the connection without dragging a full inbox sync — and
// a "syncing" status — in behind it, while a state-changing one still must.
// Both cases go through the real submit paths; the assertion is on the answer
// steady() gates its sync cycle with.
func TestReadOnlyCommandSkipsSync(t *testing.T) {
	m := NewManager(&config.Config{}, nil)
	a := newTestAccount(m, "A", "ok")
	events, unsubscribe := m.Subscribe()
	defer unsubscribe()

	// Nothing drains a.cmds here, so each submit gives up on its own short ctx;
	// what matters is the signal it left behind for the loop.
	send := func(ro bool) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		fn := func(*imapclient.Client) error { return nil }
		if ro {
			_ = a.submitRO(ctx, fn)
		} else {
			_ = a.submit(ctx, fn)
		}
		// What steady() does next: wait for work, and sync only if asked to.
		// The poll is long enough that a timeout can't be mistaken for a wake.
		sync := a.waitWork(context.Background(), time.Minute)
		if sync {
			a.setStatus("syncing", "")
		}
		return sync
	}

	if send(true) {
		t.Error("a read-only command asked for a sync: opening a body or running a server-side search must not resync the inbox")
	}
	if len(a.wake) != 0 {
		t.Error("a read-only command left a wake token queued: the next trip round the loop would sync anyway")
	}
	select {
	case e := <-events:
		t.Fatalf("a read-only command broadcast %q: the spinner must not run for a command that changes nothing", e.Type)
	default:
	}
	if got := a.getStatus().State; got != "ok" {
		t.Errorf("state after a read-only command = %q, want ok", got)
	}

	if !send(false) {
		t.Error("a state-changing command did not ask for a sync: a pushed flag or move must be read back and announced")
	}
	if got := a.getStatus().State; got != "syncing" {
		t.Errorf("state after a state-changing command = %q, want syncing", got)
	}
}

// TestRefreshAllKeepsErrorVisible is requirement (2): "Refresh now" on an
// account whose worker is in connect-backoff must not repaint it as a sync that
// isn't happening — the error message is the only diagnostic there is. It must
// still genuinely retry.
func TestRefreshAllKeepsErrorVisible(t *testing.T) {
	m := NewManager(&config.Config{}, nil)
	broken := newTestAccount(m, "broken", "ok")
	broken.setStatus("error", "connect: dial tcp: refused")
	healthy := newTestAccount(m, "healthy", "ok")

	m.RefreshAll()

	if st := broken.getStatus(); st.State != "error" || st.Message == "" {
		t.Errorf("broken account after refresh = %q/%q, want its error and message intact", st.State, st.Message)
	}
	if st := healthy.getStatus(); st.State != "syncing" {
		t.Errorf("healthy account after refresh = %q, want syncing", st.State)
	}

	// ...and the refresh is not a no-op: the backing-off worker's sleep ends now
	// instead of waiting out the backoff, and the wake token is consumed rather
	// than surviving into the next session as a spurious sync.
	start := time.Now()
	if cancelled := broken.sleepOrWake(context.Background(), time.Minute); cancelled {
		t.Fatal("sleepOrWake reported cancellation without a cancelled context")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("backoff took %v: an explicit refresh must cut it short", elapsed)
	}
	if len(broken.wake) != 0 {
		t.Error("wake token survived the backoff: it would fire an off-cadence sync once the account reconnects")
	}
}

// TestAnySyncing walks the overlap the "Syncing…" spinner exists for: two
// accounts, one finishing while the other is still going. The aggregate must
// stay true until the last one is done — and every transition must reach SSE
// subscribers, since the spinner is only ever told, never asks.
func TestAnySyncing(t *testing.T) {
	m := NewManager(&config.Config{}, nil)
	a, b := newTestAccount(m, "A", "ok"), newTestAccount(m, "B", "ok")
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
