// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"sync"
	"time"
)

// limiter is a fixed-window counter keyed by an arbitrary string (an IP or an
// email address). It exists to make /retrieve useless for enumerating who has
// bought a licence, and for using this service as a mail cannon.
//
// ponytail: in-memory, so it resets on restart and does not survive a second
// replica. This runs as one container on one VPS; move the counters into the
// SQLite file if that ever stops being true.
type limiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	max    int
	window time.Duration
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{hits: map[string][]time.Time{}, max: max, window: window}
}

// allow records an attempt and reports whether it is within budget.
func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-l.window)
	if len(l.hits) > 4096 {
		l.sweep(cutoff)
	}
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// sweep drops keys whose attempts have all aged out, so a flood of distinct IPs
// cannot grow the map without bound. Caller holds the lock.
func (l *limiter) sweep(cutoff time.Time) {
	for k, ts := range l.hits {
		if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
			delete(l.hits, k)
		}
	}
}
