// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"strings"
	"testing"
)

func TestPrefsDefaultsWhenEmpty(t *testing.T) {
	s := open(t)
	p := s.GetPrefs()
	if p.MarkReadDelay != 0 || p.SyncIntervalMin != 5 || !p.ShowAvatar {
		t.Fatalf("unexpected defaults: %+v", p)
	}
	if !p.PreviewDesktop || p.PreviewDesktopLines != 1 || !p.PreviewMobile || p.PreviewMobileLines != 1 {
		t.Fatalf("unexpected preview defaults: %+v", p)
	}
	if p.ShowListLabels {
		t.Fatalf("ShowListLabels should default off: %+v", p)
	}
	if p.AvatarShape != "circle" {
		t.Fatalf("AvatarShape should default to circle: %+v", p)
	}
	if len(p.AccountColors) != 0 {
		t.Fatalf("expected no account colors, got %v", p.AccountColors)
	}
	if p.SwipeLeftAction != "none" || p.SwipeRightAction != "unread" {
		t.Fatalf("unexpected swipe defaults: SwipeLeftAction=%q, SwipeRightAction=%q", p.SwipeLeftAction, p.SwipeRightAction)
	}
}

func TestPrefsRoundTrip(t *testing.T) {
	s := open(t)
	want := Prefs{
		MarkReadDelay:       3,
		SyncIntervalMin:     10,
		PreviewDesktop:      false,
		PreviewDesktopLines: 4,
		PreviewMobile:       true,
		PreviewMobileLines:  5,
		ShowAvatar:          false,
		ShowListLabels:      true,
		AvatarShape:         "square",
		AccountColors:       map[string]string{"work": "#6366f1", "personal": "#22c55e"},
	}
	if err := s.SavePrefs(want); err != nil {
		t.Fatal(err)
	}
	got := s.GetPrefs()
	if got.MarkReadDelay != want.MarkReadDelay || got.SyncIntervalMin != want.SyncIntervalMin ||
		got.ShowAvatar != want.ShowAvatar ||
		got.ShowListLabels != want.ShowListLabels || got.AvatarShape != want.AvatarShape {
		t.Fatalf("scalars mismatch: got %+v want %+v", got, want)
	}
	if got.PreviewDesktop != want.PreviewDesktop || got.PreviewDesktopLines != want.PreviewDesktopLines ||
		got.PreviewMobile != want.PreviewMobile || got.PreviewMobileLines != want.PreviewMobileLines {
		t.Fatalf("preview prefs mismatch: got %+v want %+v", got, want)
	}
	if got.AccountColors["work"] != "#6366f1" || got.AccountColors["personal"] != "#22c55e" {
		t.Fatalf("colors mismatch: %v", got.AccountColors)
	}
}

// TestReplyQuotePrefs: quoting the whole original is what a mail client does
// unless told otherwise, and the stored choice has to survive a round trip.
func TestReplyQuotePrefs(t *testing.T) {
	s := open(t)
	if p := s.GetPrefs(); p.ReplyQuote != "all" || p.ReplyQuoteLines != DefaultReplyQuoteLines {
		t.Fatalf("unexpected quoting defaults: %q / %d", p.ReplyQuote, p.ReplyQuoteLines)
	}
	if err := s.SavePrefs(Prefs{ReplyQuote: "lines", ReplyQuoteLines: 4}); err != nil {
		t.Fatal(err)
	}
	if p := s.GetPrefs(); p.ReplyQuote != "lines" || p.ReplyQuoteLines != 4 {
		t.Fatalf("round trip: %q / %d", p.ReplyQuote, p.ReplyQuoteLines)
	}
	if err := s.SavePrefs(Prefs{ReplyQuote: "none", ReplyQuoteLines: 4}); err != nil {
		t.Fatal(err)
	}
	if p := s.GetPrefs(); p.ReplyQuote != "none" {
		t.Fatalf("ReplyQuote = %q, want none", p.ReplyQuote)
	}
	// A hand-edited row naming something that isn't a choice falls back rather
	// than reaching the compose path.
	if err := s.setSetting("reply_quote", "everything"); err != nil {
		t.Fatal(err)
	}
	if p := s.GetPrefs(); p.ReplyQuote != "all" {
		t.Fatalf("junk value survived: %q", p.ReplyQuote)
	}
}

func TestAIModelFor(t *testing.T) {
	s := open(t)
	// Nothing configured: every feature runs on the built-in default.
	c := s.GetAppConfig()
	for _, f := range []AIFeature{AICompose, AIOptions, AIRefine, AISummarize} {
		if got := c.ModelFor(f); got != defaultAIModel {
			t.Fatalf("%s model = %q, want the default", f, got)
		}
	}
	// One override set: only that feature moves off the default model.
	c.AIModel = "big/model"
	c.AISummarizeModel = "cheap/model"
	if err := s.SaveAppConfig(c); err != nil {
		t.Fatal(err)
	}
	c = s.GetAppConfig()
	if got := c.ModelFor(AISummarize); got != "cheap/model" {
		t.Errorf("summarize model = %q", got)
	}
	if got := c.ModelFor(AICompose); got != "big/model" {
		t.Errorf("compose model = %q, want the default model", got)
	}
	// Cleared override falls back to the default model again.
	c.AISummarizeModel = ""
	if err := s.SaveAppConfig(c); err != nil {
		t.Fatal(err)
	}
	if got := s.GetAppConfig().ModelFor(AISummarize); got != "big/model" {
		t.Errorf("cleared override: summarize model = %q", got)
	}
}

func TestAIConfigUpgradeFromFlatKeys(t *testing.T) {
	// An install configured before the per-feature split: only the old flat keys
	// exist. They must keep working as the shared defaults.
	s := open(t)
	for k, v := range map[string]string{
		"ai_model": "old/model", "ai_tone": "friendly", "ai_brevity": "concise",
		"ai_language": "Italian", "ai_reply_options": "5", "ai_summary_level": "detailed",
	} {
		if err := s.setSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
	c := s.GetAppConfig()
	if c.AIModel != "old/model" || c.AITone != "friendly" || c.AIBrevity != "concise" ||
		c.AILanguage != "Italian" || c.AIReplyOptions != 5 || c.AISummaryLevel != "detailed" {
		t.Fatalf("old settings not preserved: %+v", c)
	}
	for _, f := range []AIFeature{AICompose, AIOptions, AIRefine, AISummarize} {
		if got := c.ModelFor(f); got != "old/model" {
			t.Errorf("%s model = %q, want the previously configured model", f, got)
		}
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
