// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"github.com/mattmezza/mimux/internal/store"
	"strings"
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

func TestRawAttachmentRejectsDeclaredOversizeBeforeFetch(t *testing.T) {
	m := &Manager{}
	_, err := m.RawAttachment(context.Background(), &store.Message{Size: MaxAttachTotal + 1})
	if err == nil || !strings.Contains(err.Error(), "attachment limit") {
		t.Fatalf("oversize preflight: %v", err)
	}
}

func TestValidateCombinedAttachmentSize(t *testing.T) {
	atts := []OutAttachment{{Data: make([]byte, MaxAttachTotal)}, {Data: []byte("x")}}
	if err := ValidateAttachmentSize(atts[:1]); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAttachmentSize(atts); err == nil {
		t.Fatal("combined size beyond cap accepted")
	}
}
