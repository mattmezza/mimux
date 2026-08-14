package mail

import (
	"reflect"
	"testing"
)

func TestMessageLabels(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{`\Inbox \Important`, nil}, // system labels dropped
		{`\Inbox "Work" "Receipts"`, []string{"Work", "Receipts"}}, // user labels kept, quotes stripped
		{`Personal Personal`, []string{"Personal"}},                // deduped
		{`\Sent Project_X`, []string{"Project X"}},                 // underscore→space
		{`\Starred Unread`, nil},                                   // "Unread" is a system name after normalize
		{`\Seen \Triaged`, []string{"Triaged"}},                    // system flag hidden, custom \-keyword shown
		{`\CategoryPromotions Triaged`, []string{"Triaged"}},       // Gmail system label hidden, bare keyword shown
		{`$Junk $Triaged`, []string{"Triaged"}},                    // Gmail internal $-keyword hidden, custom $-keyword shown
	}
	for _, c := range cases {
		if got := MessageLabels(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("MessageLabels(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestAddRemoveLabel: adding a user label must round-trip through the display
// form (spaces ↔ underscores), never duplicate, never touch Gmail's \System
// tokens, and remove by whatever form the pill showed.
func TestAddRemoveLabel(t *testing.T) {
	cases := []struct {
		raw, label string
		add        bool
		want       string
	}{
		{"", "Work", true, "Work"},
		{`\Inbox`, "Work", true, `\Inbox Work`},                      // system tokens preserved
		{`\Inbox Work`, "Work", true, `\Inbox Work`},                 // already there
		{`\Inbox Work`, "work", true, `\Inbox Work`},                 // case-insensitive dedup
		{"", "  Project   X  ", true, "Project_X"},                   // spaces → the stored convention
		{"Work", "Inbox", true, "Work"},                              // system name refused
		{"Work", `"quoted"`, true, "Work quoted"},                    // quotes/backslashes stripped
		{`\Inbox Project_X Work`, "Project X", false, `\Inbox Work`}, // removed by display form
		{`\Inbox "Project_X"`, "Project_X", false, `\Inbox`},         // and by stored form, quoted
		{"Work", "Nope", false, "Work"},                              // absent: unchanged
		{"Work", "  ", false, "Work"},                                // empty: unchanged
	}
	for _, c := range cases {
		got := RemoveLabel(c.raw, c.label)
		if c.add {
			got = AddLabel(c.raw, c.label)
		}
		if got != c.want {
			t.Errorf("add=%v %q %q = %q, want %q", c.add, c.raw, c.label, got, c.want)
		}
	}
	// A full add → display → remove round-trip leaves nothing behind.
	raw := AddLabel(`\Inbox`, "Project X")
	shown := MessageLabels(raw)
	if !reflect.DeepEqual(shown, []string{"Project X"}) {
		t.Fatalf("display after add = %v", shown)
	}
	if got := RemoveLabel(raw, shown[0]); got != `\Inbox` {
		t.Fatalf("round-trip left %q", got)
	}
}

// TestMergeLabels covers sync's ingestion path: a keyword flag reported by
// the server becomes a visible label, a system flag never does, and a label
// applied locally (which the server can't echo back — see Manager.SetLabel)
// survives being merged against a fresh flag set instead of being replaced.
func TestMergeLabels(t *testing.T) {
	cases := []struct {
		existing string
		flags    []string
		want     string
	}{
		// keyword flag from the server becomes a (visible, once displayed) label
		{"", []string{`\Seen`, "Triaged"}, "Triaged"},
		// system flags alone add nothing
		{"", []string{`\Seen`, `\Answered`, `\Flagged`}, ""},
		// a user-added label (no sigil, stored by LabelToken) survives a sync
		// that reports only system flags — never wiped by the merge
		{"Work", []string{`\Seen`}, "Work"},
		// … and survives alongside a newly-observed keyword too
		{"Work", []string{`\Seen`, "Triaged"}, "Work Triaged"},
		// already present (any sigil form) is not duplicated
		{"Triaged", []string{"Triaged"}, "Triaged"},
		{`\Triaged`, []string{"Triaged"}, `\Triaged`},
	}
	for _, c := range cases {
		if got := MergeLabels(c.existing, c.flags); got != c.want {
			t.Errorf("MergeLabels(%q, %v) = %q, want %q", c.existing, c.flags, got, c.want)
		}
	}
}

func TestAllLabels(t *testing.T) {
	got := AllLabels([]string{`\Inbox Work`, "Project_X Work", "", `\Sent`})
	if want := []string{"Project X", "Work"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AllLabels = %v, want %v", got, want)
	}
}
