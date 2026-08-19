// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"log/slog"
	"sync"
	"time"
)

// hub is a tiny fan-out broadcaster. Subscribers pick their loss policy:
// browsers are lossy (a stale tab refreshes itself), the webhook engine is
// durable (a dropped event is a delivery that never happens and is never
// retried).
type hub struct {
	mu sync.Mutex
	// value: durable. A durable subscriber gets the bigger buffer and a
	// broadcaster willing to wait for it.
	subs map[chan Event]bool
}

// lossyBuffer is a browser's backlog: enough to ride out a swap, small enough
// that a frozen tab costs nothing.
const lossyBuffer = 8

// durableBuffer is a durable subscriber's backlog. 256 covers a full sync cycle
// of a busy mailbox landing while the consumer is mid-work.
const durableBuffer = 256

// durableWait is how long a broadcast blocks for a durable subscriber whose
// buffer is full before giving up on it. Bounded because the sync engine is on
// the other end of this call and must not be wedged by a stuck consumer.
const durableWait = 2 * time.Second

func newHub() *hub { return &hub{subs: map[chan Event]bool{}} }

// subscribe returns a lossy subscription: when it falls behind, the oldest
// event is dropped for it. Right for anything that can re-read the world.
func (h *hub) subscribe() (<-chan Event, func()) { return h.add(lossyBuffer, false) }

// subscribeDurable returns a subscription the broadcaster will wait on rather
// than drop from. For consumers where an event is an action to take once.
func (h *hub) subscribeDurable() (<-chan Event, func()) { return h.add(durableBuffer, true) }

func (h *hub) add(buffer int, durable bool) (<-chan Event, func()) {
	ch := make(chan Event, buffer)
	h.mu.Lock()
	h.subs[ch] = durable
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// broadcast hands the event to every subscriber.
//
// ponytail: the sends happen under the lock, so one full durable subscriber
// stalls every broadcaster for up to durableWait. That is the price of closing
// the channel in unsubscribe (a send outside the lock could hit a closed
// channel); a per-subscriber sender goroutine is the upgrade if a second
// durable consumer ever appears.
func (h *hub) broadcast(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch, durable := range h.subs {
		select {
		case ch <- e:
			continue
		default:
		}
		if durable {
			t := time.NewTimer(durableWait)
			select {
			case ch <- e:
			case <-t.C:
				// Loud: this is an event a consumer was promised and is not
				// getting, and nothing replays it.
				slog.Error("hub: durable subscriber is too far behind, dropping an event",
					"type", e.Type, "waited", durableWait)
			}
			t.Stop()
			continue
		}
		// Full: a subscriber has fallen behind (a frozen background tab
		// stops draining its socket). Drop the OLDEST rather than this one —
		// these events are "something changed, re-read it", so the newest is
		// the only one that has to arrive. Dropping the newest, as this used
		// to, is indistinguishable from nothing having happened, and there is
		// no reconnect to recover from it: the tab just stays stale.
		// NOTE: still lossy in the middle. Replaying the gap needs a per
		// subscriber cursor and a retained log — not worth it while every
		// event is idempotent and the client also refreshes on becoming
		// visible.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- e:
		default:
		}
	}
}
