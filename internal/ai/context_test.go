// SPDX-License-Identifier: AGPL-3.0-only
package ai

import (
	"strings"
	"testing"
	"time"
)

func at(min int) time.Time { return time.Date(2026, 7, 20, 10, min, 0, 0, time.UTC) }

// TestBuildContext pins the shape the reply features are prompted with: the
// message being replied to is there in full, the rest of the thread is there
// around it, and it reads oldest to newest.
func TestBuildContext(t *testing.T) {
	orig := Msg{From: "Alice <alice@example.com>", Date: at(30), Subject: "Lunch", Text: "Can you make Thursday?"}
	// Deliberately unsorted: the caller hands over whatever the thread closure
	// found, and the order is this function's problem.
	thread := []Msg{
		{From: "Bob <bob@example.com>", Date: at(20), Subject: "Lunch", Text: "Tuesday is bad for me."},
		{From: "Alice <alice@example.com>", Date: at(10), Subject: "Lunch", Text: "Lunch on Tuesday?"},
	}
	got := BuildContext(orig, thread)

	for _, want := range []string{"Can you make Thursday?", "Tuesday is bad for me.", "Lunch on Tuesday?", "Alice <alice@example.com>", "Subject: Lunch"} {
		if !strings.Contains(got, want) {
			t.Errorf("context is missing %q:\n%s", want, got)
		}
	}
	first, second, last := strings.Index(got, "Lunch on Tuesday?"), strings.Index(got, "Tuesday is bad"), strings.Index(got, "Thursday?")
	if first >= second || second >= last {
		t.Errorf("conversation is out of order (%d, %d, %d):\n%s", first, second, last, got)
	}
	if !strings.Contains(got[last-200:], "The message I am replying to") {
		t.Errorf("the message being replied to is not labelled as such:\n%s", got)
	}
}

// TestBuildContextBudget: a thread longer than the budget keeps the message
// being replied to plus the most recent exchanges, and drops the oldest.
func TestBuildContextBudget(t *testing.T) {
	orig := Msg{From: "alice@example.com", Date: at(59), Text: "newest of all"}
	var thread []Msg
	for i := range 40 {
		thread = append(thread, Msg{
			From: "bob@example.com", Date: at(i),
			Text: "message " + strings.Repeat("x", 900) + " number " + string(rune('a'+i%26)),
		})
	}
	got := BuildContext(orig, thread)
	if len(got) > MaxContextChars+500 {
		t.Errorf("context is %d chars, over the %d budget", len(got), MaxContextChars)
	}
	if !strings.Contains(got, "newest of all") {
		t.Error("dropped the message being replied to")
	}
	// The oldest of 40 long messages cannot have survived a 12k budget.
	if strings.Count(got, "--- Earlier in this conversation ---") >= len(thread) {
		t.Errorf("kept the whole thread: %d blocks", strings.Count(got, "--- Earlier"))
	}
}

// A single enormous message is cut rather than sent whole, and says so.
func TestBuildContextTruncatesOneMessage(t *testing.T) {
	orig := Msg{From: "alice@example.com", Date: at(1), Text: strings.Repeat("y", MaxContextChars*2)}
	got := BuildContext(orig, nil)
	if len(got) > MaxContextChars+500 {
		t.Errorf("context is %d chars, over the %d budget", len(got), MaxContextChars)
	}
	if !strings.HasSuffix(strings.TrimSpace(got), truncMarker) {
		t.Errorf("truncated message is not marked: %q", got[len(got)-80:])
	}
}

// TestReplyPromptsCarryTheConversation is the end of the chain the compose
// window feeds: whatever BuildContext assembled has to survive into the prompt
// both reply features actually send.
func TestReplyPromptsCarryTheConversation(t *testing.T) {
	ctx := BuildContext(
		Msg{From: "alice@example.com", Date: at(30), Subject: "Lunch", Text: "Can you make Thursday?"},
		[]Msg{{From: "bob@example.com", Date: at(10), Subject: "Lunch", Text: "Tuesday is bad for me."}},
	)
	draft, err := draftPrompt(ctx, "accept politely", false)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := optionsPrompt(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"draft": draft, "options": opts} {
		if !strings.Contains(p, "Can you make Thursday?") {
			t.Errorf("%s prompt is missing the message being replied to:\n%s", name, p)
		}
		if !strings.Contains(p, "Tuesday is bad for me.") {
			t.Errorf("%s prompt is missing the thread:\n%s", name, p)
		}
	}
	if !strings.Contains(draft, "accept politely") {
		t.Errorf("draft prompt lost the direction:\n%s", draft)
	}
}

// An empty thread (a message that starts its own conversation) is just the one
// message, with no dangling header.
func TestBuildContextNoThread(t *testing.T) {
	got := BuildContext(Msg{From: "alice@example.com", Date: at(1), Text: "hello"}, []Msg{{Text: "   "}})
	if strings.Contains(got, "Earlier in this conversation") {
		t.Errorf("rendered a block for an empty thread message:\n%s", got)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("lost the message:\n%s", got)
	}
}

// TestBuildThreadContext pins the shape the whole-thread summarize feature is
// fed: every recent message is present in full, the earlier ones are there
// too, and the whole thing reads oldest to newest regardless of the order the
// two slices are handed in.
func TestBuildThreadContext(t *testing.T) {
	recent := []Msg{
		{From: "Alice <alice@example.com>", Date: at(30), Subject: "Lunch", Text: "How about Thursday instead?"},
		{From: "Bob <bob@example.com>", Date: at(20), Subject: "Lunch", Text: "Tuesday is bad for me."},
	}
	earlier := []Msg{{From: "Alice <alice@example.com>", Date: at(10), Subject: "Lunch", Text: "Lunch on Tuesday?"}}
	got := BuildThreadContext(recent, earlier)

	for _, want := range []string{"Lunch on Tuesday?", "Tuesday is bad for me.", "How about Thursday instead?", "Bob <bob@example.com>"} {
		if !strings.Contains(got, want) {
			t.Errorf("thread context is missing %q:\n%s", want, got)
		}
	}
	first := strings.Index(got, "Lunch on Tuesday?")
	second := strings.Index(got, "Tuesday is bad for me.")
	last := strings.Index(got, "How about Thursday")
	if first < 0 || second < 0 || last < 0 || first >= second || second >= last {
		t.Errorf("conversation is out of order (%d, %d, %d):\n%s", first, second, last, got)
	}
	if !strings.Contains(got, "--- Recent message in this conversation ---") {
		t.Errorf("recent messages are not labelled as such:\n%s", got)
	}
	if !strings.Contains(got, "--- Earlier in this conversation ---") {
		t.Errorf("earlier messages are not labelled as such:\n%s", got)
	}
}

// TestBuildThreadContextBudget: a thread longer than the budget keeps the
// recent messages plus as much earlier context as fits, and drops the oldest,
// same economics as BuildContext.
func TestBuildThreadContextBudget(t *testing.T) {
	recent := []Msg{
		{From: "alice@example.com", Date: at(59), Text: "newest of all"},
		{From: "alice@example.com", Date: at(58), Text: "second newest"},
	}
	var earlier []Msg
	for i := range 40 {
		earlier = append(earlier, Msg{
			From: "bob@example.com", Date: at(i),
			Text: "message " + strings.Repeat("x", 900) + " number " + string(rune('a'+i%26)),
		})
	}
	got := BuildThreadContext(recent, earlier)
	if len(got) > MaxContextChars+500 {
		t.Errorf("thread context is %d chars, over the %d budget", len(got), MaxContextChars)
	}
	if !strings.Contains(got, "newest of all") || !strings.Contains(got, "second newest") {
		t.Error("dropped a recent message")
	}
	// The oldest of 40 long earlier messages cannot have survived a 12k budget
	// shared with two full recent messages.
	if strings.Count(got, "--- Earlier in this conversation ---") >= len(earlier) {
		t.Errorf("kept the whole earlier thread: %d blocks", strings.Count(got, "--- Earlier"))
	}
}
