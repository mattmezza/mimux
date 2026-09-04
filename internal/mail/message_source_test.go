// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"testing"
)

func TestMessageFilename(t *testing.T) {
	tests := []struct{ subject, want string }{
		{"Quarterly report", "Quarterly report.eml"},
		{` Re: bad/name:*?"<>| `, `Re_ bad_name_______.eml`},
		{"...", "message-42.eml"},
		{"", "message-42.eml"},
	}
	for _, tt := range tests {
		if got := MessageFilename(tt.subject, 42); got != tt.want {
			t.Errorf("MessageFilename(%q) = %q, want %q", tt.subject, got, tt.want)
		}
	}
	long := MessageFilename(string(make([]rune, 200)), 42)
	if len([]rune(long)) > 124 {
		t.Errorf("long filename has %d runes", len([]rune(long)))
	}
}

func TestRawReturnsExactMessage(t *testing.T) {
	m, msg := headerAccount(t)
	raw, err := m.Raw(context.Background(), msg)
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	if string(raw) != foldedRaw {
		t.Errorf("Raw changed the source:\n got %q\nwant %q", raw, foldedRaw)
	}
}
