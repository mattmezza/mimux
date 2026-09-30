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
	if !p.DaySeparators {
		t.Fatalf("DaySeparators should default on: %+v", p)
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
	if p.Keybindings["next"] != "j" || p.Keybindings["goto_inbox"] != "g i" || p.Keybindings["goto_account_9"] != "9" {
		t.Fatalf("unexpected keybinding defaults: %+v", p.Keybindings)
	}
}

func TestKeybindingsRoundTripAndInvalidImportFallback(t *testing.T) {
	s := open(t)
	p := s.GetPrefs()
	p.Keybindings["archive"] = "x"
	if err := s.SavePrefs(p); err != nil {
		t.Fatal(err)
	}
	if got := s.GetPrefs().Keybindings["archive"]; got != "x" {
		t.Fatalf("archive = %q", got)
	}
	if err := s.setSetting("keybindings", `{"archive":"Enter","unknown":"z"}`); err != nil {
		t.Fatal(err)
	}
	got := s.GetPrefs().Keybindings
	if got["archive"] != "e" || got["unknown"] != "" {
		t.Fatalf("invalid imported bindings survived: %+v", got)
	}
}

func TestValidateKeybinding(t *testing.T) {
	for _, valid := range []string{"x", "X", "?", "Space", "g i"} {
		if err := ValidateKeybinding(valid); err != nil {
			t.Errorf("%q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "Enter", "Tab", "ArrowUp", "Ctrl+x", "g i x", " "} {
		if err := ValidateKeybinding(invalid); err == nil {
			t.Errorf("%q unexpectedly valid", invalid)
		}
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
		DaySeparators:       true,
		AvatarShape:         "square",
		AccountColors:       map[string]string{"work": "#6366f1", "personal": "#22c55e"},
	}
	if err := s.SavePrefs(want); err != nil {
		t.Fatal(err)
	}
	got := s.GetPrefs()
	if got.MarkReadDelay != want.MarkReadDelay || got.SyncIntervalMin != want.SyncIntervalMin ||
		got.ShowAvatar != want.ShowAvatar ||
		got.ShowListLabels != want.ShowListLabels || got.DaySeparators != want.DaySeparators || got.AvatarShape != want.AvatarShape {
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
	for _, f := range []AIFeature{AICompose, AIOptions, AIRefine, AISummarize, AIThreadSummarize} {
		if got := c.ModelFor(f); got != defaultAIModel {
			t.Fatalf("%s model = %q, want the default", f, got)
		}
	}
	// One override set: only that feature moves off the default model.
	c.AIModel = "big/model"
	c.AISummarizeModel = "cheap/model"
	c.AIThreadSummarizeModel = "thread/model"
	c.AIThreadSummaryLevel = "detailed"
	c.AIThreadSummaryEnabled = false
	if err := s.SaveAppConfig(c); err != nil {
		t.Fatal(err)
	}
	c = s.GetAppConfig()
	if got := c.ModelFor(AISummarize); got != "cheap/model" {
		t.Errorf("summarize model = %q", got)
	}
	if got := c.ModelFor(AIThreadSummarize); got != "thread/model" || c.AIThreadSummaryLevel != "detailed" || c.AIThreadSummaryEnabled {
		t.Errorf("thread summary config not round-tripped: model=%q level=%q enabled=%v", got, c.AIThreadSummaryLevel, c.AIThreadSummaryEnabled)
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

func TestValidReasoningEffort(t *testing.T) {
	// The contract is this fixed set: the empty inherit value plus the seven
	// OpenRouter efforts, "none" (off) included.
	want := map[string]bool{"": true, "max": true, "xhigh": true, "high": true,
		"medium": true, "low": true, "minimal": true, "none": true}
	seen := map[string]bool{}
	for _, e := range AllReasoningEfforts {
		if seen[e.ID] {
			t.Errorf("effort %q listed twice", e.ID)
		}
		seen[e.ID] = true
		if e.Label == "" {
			t.Errorf("effort %q has no label", e.ID)
		}
		if got := ValidReasoningEffort(e.ID, "junk"); got != e.ID {
			t.Errorf("ValidReasoningEffort(%q) = %q", e.ID, got)
		}
	}
	// The set is checked against a literal list, not against itself, so a
	// value silently dropped from AllReasoningEfforts fails here.

	if len(seen) != len(want) {
		t.Fatalf("effort set = %v, want %v", seen, want)
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("effort %q missing from AllReasoningEfforts", id)
		}
	}
	// Anything else is replaced by the caller's default, never stored.
	for _, bad := range []string{"enormous", "NONE", "high ", "true"} {
		if got := ValidReasoningEffort(bad, "medium"); got != "medium" {
			t.Errorf("ValidReasoningEffort(%q) = %q, want medium", bad, got)
		}
	}
}

func TestAIReasoningFor(t *testing.T) {
	s := open(t)
	// Nothing configured: no reasoning parameter is sent anywhere, which is
	// exactly today's behaviour and makes the whole feature opt-in.
	c := s.GetAppConfig()
	if c.AIReasoningEffort != "" {
		t.Fatalf("default reasoning effort = %q, want blank", c.AIReasoningEffort)
	}
	for _, f := range []AIFeature{AICompose, AIOptions, AIRefine, AISummarize, AIThreadSummarize} {
		if got := c.ReasoningFor(f); got != "" {
			t.Fatalf("%s reasoning = %q, want blank", f, got)
		}
	}
	// A global default reaches every feature that does not pin its own; a
	// per-feature override wins, "none" (off) included.
	c.AIReasoningEffort = "medium"
	c.AIThreadSummarizeReasoningEffort = "high"
	c.AIRefineReasoningEffort = "none"
	if err := s.SaveAppConfig(c); err != nil {
		t.Fatal(err)
	}
	c = s.GetAppConfig()
	for f, want := range map[AIFeature]string{
		AICompose: "medium", AIOptions: "medium", AISummarize: "medium",
		AIThreadSummarize: "high", AIRefine: "none",
	} {
		if got := c.ReasoningFor(f); got != want {
			t.Errorf("%s reasoning = %q, want %q", f, got, want)
		}
	}
	// Invalid values are dropped on write and on read: they can never reach a
	// JSON request body.
	c.AIReasoningEffort = "enormous"
	c.AIOptionsReasoningEffort = "  "
	if err := s.SaveAppConfig(c); err != nil {
		t.Fatal(err)
	}
	if got := s.GetAppConfig(); got.AIReasoningEffort != "" || got.AIOptionsReasoningEffort != "" {
		t.Errorf("invalid efforts stored: global=%q options=%q", got.AIReasoningEffort, got.AIOptionsReasoningEffort)
	}
	if err := s.setSetting("ai_summarize_reasoning", "nope"); err != nil {
		t.Fatal(err)
	}
	if got := s.GetAppConfig().AISummarizeReasoningEffort; got != "" {
		t.Errorf("hand-edited bad row survived the read: %q", got)
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
	for _, f := range []AIFeature{AICompose, AIOptions, AIRefine, AISummarize, AIThreadSummarize} {
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
