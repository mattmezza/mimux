package mail

import (
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
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
