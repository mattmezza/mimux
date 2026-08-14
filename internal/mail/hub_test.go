package mail

import (
	"strconv"
	"testing"
)

// TestBroadcastKeepsNewest pins the drop policy for a subscriber that has
// fallen behind (a frozen background tab stops draining its socket, so the SSE
// writer stops draining the hub). Every event is "something changed, re-read
// it", so losing an old one costs nothing and losing the newest costs
// everything: it is indistinguishable from nothing having happened, and there
// is no reconnect to recover from it.
func TestBroadcastKeepsNewest(t *testing.T) {
	h := newHub()
	ch, unsubscribe := h.subscribe()
	defer unsubscribe()

	newest := ""
	for i := 0; i < cap(ch)+3; i++ {
		newest = strconv.Itoa(i)
		h.broadcast(Event{Type: "new-mail", Data: newest})
	}

	var got []string
	for len(ch) > 0 {
		got = append(got, (<-ch).Data)
	}
	if len(got) != cap(ch) {
		t.Fatalf("buffered %d events, want the channel's %d", len(got), cap(ch))
	}
	if last := got[len(got)-1]; last != newest {
		t.Errorf("newest event delivered = %q, want the last one broadcast: a dropped tail leaves the tab stale forever", last)
	}
}
