package store

import (
	"strings"
	"testing"
)

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

func TestSplitQuickActions(t *testing.T) {
	// New format: placement + order preserved, unknown ids and dupes dropped.
	bar, menu := SplitQuickActions("archive=bar,reply=bar,star=menu,bogus=bar,reply=menu,delete=menu")
	if strings.Join(bar, ",") != "archive,reply" || strings.Join(menu, ",") != "star,delete" {
		t.Fatalf("split: bar=%v menu=%v", bar, menu)
	}
	// Legacy format (pre-placement CSV): fixed bar, stored ids become the menu.
	bar, menu = SplitQuickActions("dark,star,unread,delete")
	if strings.Join(bar, ",") != "reply,unread,archive" || strings.Join(menu, ",") != "dark,star,delete" {
		t.Fatalf("legacy: bar=%v menu=%v", bar, menu)
	}
	// Default round-trips.
	bar, menu = SplitQuickActions(defaultQuickActions())
	if JoinQuickActions(bar, menu) != defaultQuickActions() {
		t.Fatalf("default did not round-trip: bar=%v menu=%v", bar, menu)
	}
	// Everything hidden is a valid (empty) preference.
	if bar, menu = SplitQuickActions(""); len(bar)+len(menu) != 0 {
		t.Fatalf("empty pref: bar=%v menu=%v", bar, menu)
	}
}
