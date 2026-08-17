// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"container/list"
	"sync"
)

// bodyLRU is a tiny fixed-capacity LRU cache for fetched message bodies, keyed
// by the local message id. Hand-rolled (map + list) — no dependency needed.
type bodyLRU struct {
	mu  sync.Mutex
	cap int
	ll  *list.List // front = most recently used
	m   map[int64]*list.Element
}

type lruEntry struct {
	key  int64
	body *messageBody
}

func newBodyLRU(capacity int) *bodyLRU {
	if capacity < 1 {
		capacity = 1
	}
	return &bodyLRU{cap: capacity, ll: list.New(), m: map[int64]*list.Element{}}
}

func (c *bodyLRU) get(key int64) (*messageBody, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*lruEntry).body, true
	}
	return nil, false
}

func (c *bodyLRU) put(key int64, body *messageBody) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*lruEntry).body = body
		return
	}
	c.m[key] = c.ll.PushFront(&lruEntry{key: key, body: body})
	if c.ll.Len() > c.cap {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.m, oldest.Value.(*lruEntry).key)
		}
	}
}
