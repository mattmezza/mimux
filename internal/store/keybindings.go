// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Keybinding describes one customizable global shortcut. Binding is the
// shipped default; two-key values are sequences, not modifier chords.
type Keybinding struct {
	ID, Label, Description, Category, Binding string
}

func (k Keybinding) Sequence() bool { return strings.Contains(k.Binding, " ") }

var AllKeybindings = []Keybinding{
	{"focus_list", "Focus message list", "Move keyboard focus to the message list", "Navigation", "h"},
	{"focus_reading", "Focus reading pane", "Move keyboard focus to the open message", "Navigation", "l"},
	{"next", "Next message", "Select the next message, or scroll down while reading", "Navigation", "j"},
	{"previous", "Previous message", "Select the previous message, or scroll up while reading", "Navigation", "k"},
	{"last", "Last message", "Select the last message, or scroll to the bottom", "Navigation", "J"},
	{"first", "First message", "Select the first message, or scroll to the top", "Navigation", "K"},
	{"open", "Open message", "Open the selected message", "Navigation", "o"},
	{"toggle_thread", "Expand or collapse thread", "Toggle the selected conversation", "Navigation", "Space"},
	{"cycle_filter", "Cycle quick filter", "Cycle All, Unread, and Starred", "Navigation", "f"},
	{"search", "Search", "Focus the search field", "Navigation", "/"},
	{"mark_read", "Mark read", "Mark the selected message as read", "Actions", "r"},
	{"mark_unread", "Mark unread", "Mark the selected message as unread", "Actions", "u"},
	{"star", "Star or unstar", "Toggle the selected message's star", "Actions", "s"},
	{"archive", "Archive", "Archive the selected message", "Actions", "e"},
	{"delete", "Delete", "Delete the selected message", "Actions", "d"},
	{"delete_alt", "Delete (alternate)", "Alternate delete shortcut", "Actions", "#"},
	{"spam", "Mark as spam", "Move the selected message to spam", "Actions", "!"},
	{"compose", "New message", "Start composing a message", "Compose", "c"},
	{"reply", "Reply", "Reply to the open message", "Compose", "R"},
	{"reply_all", "Reply all", "Reply to everyone on the open message", "Compose", "A"},
	{"forward", "Forward", "Forward the open message", "Compose", "F"},
	{"goto_inbox", "Go to inbox", "Open the current account's inbox", "Go to", "g i"},
	{"goto_sent", "Go to sent", "Open the first account's Sent folder", "Go to", "g t"},
	{"goto_starred", "Go to starred", "Search starred messages", "Go to", "g s"},
	{"goto_drafts", "Go to drafts", "Open drafts", "Go to", "g d"},
	{"goto_unified", "Go to unified inbox", "Open all inboxes", "Go to", "0"},
	{"goto_account_1", "Go to account 1", "Open the first account inbox", "Accounts", "1"},
	{"goto_account_2", "Go to account 2", "Open the second account inbox", "Accounts", "2"},
	{"goto_account_3", "Go to account 3", "Open the third account inbox", "Accounts", "3"},
	{"goto_account_4", "Go to account 4", "Open the fourth account inbox", "Accounts", "4"},
	{"goto_account_5", "Go to account 5", "Open the fifth account inbox", "Accounts", "5"},
	{"goto_account_6", "Go to account 6", "Open the sixth account inbox", "Accounts", "6"},
	{"goto_account_7", "Go to account 7", "Open the seventh account inbox", "Accounts", "7"},
	{"goto_account_8", "Go to account 8", "Open the eighth account inbox", "Accounts", "8"},
	{"goto_account_9", "Go to account 9", "Open the ninth account inbox", "Accounts", "9"},
	{"help", "Keyboard help", "Open this shortcut guide", "Other", "?"},
}

func DefaultKeybindings() map[string]string {
	out := make(map[string]string, len(AllKeybindings))
	for _, b := range AllKeybindings {
		out[b.ID] = b.Binding
	}
	return out
}

func KeybindingIDs() map[string]bool {
	out := make(map[string]bool, len(AllKeybindings))
	for _, b := range AllKeybindings {
		out[b.ID] = true
	}
	return out
}

func KeybindingByID(id string) (Keybinding, bool) {
	for _, binding := range AllKeybindings {
		if binding.ID == id {
			return binding, true
		}
	}
	return Keybinding{}, false
}

// ValidateKeybinding accepts one printable key or an existing-style two-key
// sequence. Browser/OS control keys and modifier chords remain reserved.
func ValidateKeybinding(binding string) error {
	if binding == "Escape" || binding == "Enter" || binding == "Tab" || strings.HasPrefix(binding, "Arrow") {
		return fmt.Errorf("%s is reserved for keyboard navigation", binding)
	}
	if strings.Contains(binding, "+") || strings.HasPrefix(binding, "Ctrl") || strings.HasPrefix(binding, "Meta") || strings.HasPrefix(binding, "Alt") {
		return fmt.Errorf("modifier combinations are reserved by the browser and operating system")
	}
	parts := strings.Split(binding, " ")
	if binding == "Space" {
		parts = []string{"Space"}
	}
	if len(parts) < 1 || len(parts) > 2 {
		return fmt.Errorf("use one key or a two-key sequence")
	}
	for _, key := range parts {
		if key == "Space" {
			continue
		}
		if key == "" || utf8.RuneCountInString(key) != 1 {
			return fmt.Errorf("use a printable character")
		}
		r, _ := utf8.DecodeRuneInString(key)
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.IsSpace(r) {
			return fmt.Errorf("use a printable character")
		}
	}
	return nil
}

func ValidateKeybindings(bindings map[string]string) error {
	used := map[string]string{}
	prefixes := map[string]string{}
	for _, action := range AllKeybindings {
		binding, ok := bindings[action.ID]
		if !ok {
			return fmt.Errorf("missing %s", action.Label)
		}
		if err := ValidateKeybinding(binding); err != nil {
			return fmt.Errorf("%s: %w", action.Label, err)
		}
		if strings.Contains(binding, " ") != action.Sequence() {
			return fmt.Errorf("%s has the wrong binding shape", action.Label)
		}
		if other := used[binding]; other != "" {
			return fmt.Errorf("%s and %s both use %q", other, action.Label, binding)
		}
		parts := strings.Split(binding, " ")
		if len(parts) == 2 {
			if other := used[parts[0]]; other != "" {
				return fmt.Errorf("%s conflicts with the prefix for %s", other, action.Label)
			}
			prefixes[parts[0]] = action.Label
		} else if other := prefixes[binding]; other != "" {
			return fmt.Errorf("%s conflicts with the prefix for %s", action.Label, other)
		}
		used[binding] = action.Label
	}
	return nil
}
