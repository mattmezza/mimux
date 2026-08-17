// SPDX-License-Identifier: AGPL-3.0-only
package mail

import "testing"

func TestBodyLRUEviction(t *testing.T) {
	c := newBodyLRU(2)
	a, b, d := &messageBody{textContent: "a"}, &messageBody{textContent: "b"}, &messageBody{textContent: "d"}
	c.put(1, a)
	c.put(2, b)
	if _, ok := c.get(1); !ok {
		t.Fatal("1 should be cached")
	}
	// 1 is now most-recently-used; inserting 3 must evict 2 (the LRU).
	c.put(3, d)
	if _, ok := c.get(2); ok {
		t.Error("2 should have been evicted")
	}
	if _, ok := c.get(1); !ok {
		t.Error("1 should survive (recently used)")
	}
	if _, ok := c.get(3); !ok {
		t.Error("3 should be present")
	}
}

func TestBodyLRUUpdate(t *testing.T) {
	c := newBodyLRU(2)
	c.put(1, &messageBody{textContent: "old"})
	c.put(1, &messageBody{textContent: "new"})
	got, ok := c.get(1)
	if !ok || got.textContent != "new" {
		t.Errorf("put on existing key should update value, got %+v ok=%v", got, ok)
	}
}

func TestBodyLRUMiss(t *testing.T) {
	c := newBodyLRU(1)
	if _, ok := c.get(99); ok {
		t.Error("empty cache should miss")
	}
}
