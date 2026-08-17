// SPDX-License-Identifier: AGPL-3.0-only
package mail

import "testing"

func TestParseListUnsubscribe(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		post       string
		wantKind   UnsubKind
		wantURL    string
		wantMailto string
	}{
		{
			name:     "one-click https with post header",
			header:   "<https://example.com/unsub?id=1>, <mailto:unsub@example.com>",
			post:     "List-Unsubscribe=One-Click",
			wantKind: UnsubOneClick,
			wantURL:  "https://example.com/unsub?id=1",
		},
		{
			name:     "https link without one-click post",
			header:   "<https://example.com/unsub?id=1>",
			post:     "",
			wantKind: UnsubLink,
			wantURL:  "https://example.com/unsub?id=1",
		},
		{
			name:     "http link (not https) never one-click even with post header",
			header:   "<http://example.com/unsub>",
			post:     "List-Unsubscribe=One-Click",
			wantKind: UnsubLink,
			wantURL:  "http://example.com/unsub",
		},
		{
			name:       "mailto only",
			header:     "<mailto:unsub@example.com?subject=unsubscribe>",
			wantKind:   UnsubMailto,
			wantMailto: "mailto:unsub@example.com?subject=unsubscribe",
		},
		{
			name:     "empty header",
			header:   "",
			wantKind: UnsubNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseListUnsubscribe(tc.header, tc.post)
			if got.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.URL != tc.wantURL {
				t.Fatalf("URL = %q, want %q", got.URL, tc.wantURL)
			}
			if got.Mailto != tc.wantMailto {
				t.Fatalf("Mailto = %q, want %q", got.Mailto, tc.wantMailto)
			}
		})
	}
}

func TestParseMailtoUnsubscribe(t *testing.T) {
	to, subject, body := parseMailtoUnsubscribe("mailto:unsub@example.com?subject=Bye&body=Remove+me")
	if to != "unsub@example.com" {
		t.Errorf("to = %q", to)
	}
	if subject != "Bye" {
		t.Errorf("subject = %q", subject)
	}
	if body != "Remove me" {
		t.Errorf("body = %q", body)
	}

	to, subject, body = parseMailtoUnsubscribe("mailto:unsub@example.com")
	if to != "unsub@example.com" || subject != "Unsubscribe" || body == "" {
		t.Errorf("defaults: to=%q subject=%q body=%q", to, subject, body)
	}
}
