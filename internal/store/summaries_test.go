package store

import "testing"

func TestSummaryCacheRoundTrip(t *testing.T) {
	s := open(t)
	key := SummaryCacheKey(42, "brief")
	if _, _, ok, err := s.SummaryCached(key); err != nil || ok {
		t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
	}
	if err := s.SaveSummary(key, "- pay the invoice", true); err != nil {
		t.Fatal(err)
	}
	sum, truncated, ok, err := s.SummaryCached(key)
	if err != nil || !ok || sum != "- pay the invoice" || !truncated {
		t.Fatalf("got %q truncated=%v ok=%v err=%v", sum, truncated, ok, err)
	}
	// Another level for the same message is a separate entry.
	if _, _, ok, _ := s.SummaryCached(SummaryCacheKey(42, "oneline")); ok {
		t.Fatal("level should be part of the cache key")
	}
	// Re-summarizing overwrites in place.
	if err := s.SaveSummary(key, "- pay it today", false); err != nil {
		t.Fatal(err)
	}
	if sum, truncated, _, _ := s.SummaryCached(key); sum != "- pay it today" || truncated {
		t.Fatalf("upsert failed: %q truncated=%v", sum, truncated)
	}
}
