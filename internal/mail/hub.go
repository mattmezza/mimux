package mail

import "sync"

// hub is a tiny fan-out broadcaster for SSE subscribers. Slow subscribers drop
// events rather than block the sync engine.
type hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func newHub() *hub { return &hub{subs: map[chan Event]struct{}{}} }

func (h *hub) subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 8)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
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

func (h *hub) broadcast(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
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
}
