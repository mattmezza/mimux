package store

import "testing"

func TestPrefsDefaultsWhenEmpty(t *testing.T) {
	s := open(t)
	p := s.GetPrefs()
	if p.MarkReadDelay != 0 || p.SyncIntervalMin != 5 || p.PreviewLines != 1 || !p.ShowAvatar {
		t.Fatalf("unexpected defaults: %+v", p)
	}
	if len(p.AccountColors) != 0 {
		t.Fatalf("expected no account colors, got %v", p.AccountColors)
	}
}

func TestPrefsRoundTrip(t *testing.T) {
	s := open(t)
	want := Prefs{
		MarkReadDelay:   3,
		SyncIntervalMin: 10,
		PreviewLines:    2,
		ShowAvatar:      false,
		AccountColors:   map[string]string{"work": "#6366f1", "personal": "#22c55e"},
	}
	if err := s.SavePrefs(want); err != nil {
		t.Fatal(err)
	}
	got := s.GetPrefs()
	if got.MarkReadDelay != want.MarkReadDelay || got.SyncIntervalMin != want.SyncIntervalMin ||
		got.PreviewLines != want.PreviewLines || got.ShowAvatar != want.ShowAvatar {
		t.Fatalf("scalars mismatch: got %+v want %+v", got, want)
	}
	if got.AccountColors["work"] != "#6366f1" || got.AccountColors["personal"] != "#22c55e" {
		t.Fatalf("colors mismatch: %v", got.AccountColors)
	}
}
