// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"testing"
	"time"
)

func TestAPITokenCRUD(t *testing.T) {
	s := open(t)

	if toks, _ := s.ListAPITokens(); len(toks) != 0 {
		t.Fatalf("fresh db has %d tokens", len(toks))
	}
	if err := s.CreateAPIToken(&APIToken{Hash: "h"}); err == nil {
		t.Error("token without a label was accepted")
	}
	if err := s.CreateAPIToken(&APIToken{Label: "no hash"}); err == nil {
		t.Error("token without a hash was accepted")
	}

	tok := &APIToken{Label: "Home Assistant", Hash: "hash", Scopes: "mail:read mail:send"}
	if err := s.CreateAPIToken(tok); err != nil {
		t.Fatal(err)
	}
	if tok.ID == 0 || tok.CreatedAt.IsZero() {
		t.Fatalf("insert did not fill ID/CreatedAt: %+v", tok)
	}

	got, err := s.APITokenByID(tok.ID)
	if err != nil || got == nil {
		t.Fatalf("APITokenByID = %v, %v", got, err)
	}
	if got.Label != "Home Assistant" || got.Scopes != "mail:read mail:send" || got.Hash != "hash" {
		t.Errorf("not round-tripped: %+v", got)
	}
	if !got.Live() || got.Revoked() || got.Expired() {
		t.Errorf("fresh token should be live: %+v", got)
	}
	if !got.LastUsedAt.IsZero() || !got.ExpiresAt.IsZero() || !got.RevokedAt.IsZero() {
		t.Errorf("nullable timestamps should scan as zero: %+v", got)
	}
	if missing, _ := s.APITokenByID(9999); missing != nil {
		t.Error("nonexistent token found")
	}

	// Last used.
	used := time.Now().UTC().Truncate(time.Second)
	if err := s.TouchAPIToken(tok.ID, used); err != nil {
		t.Fatal(err)
	}
	got, _ = s.APITokenByID(tok.ID)
	if !got.LastUsedAt.Equal(used) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, used)
	}

	// Revoke: gone from the active set, still listed, and not re-stamped.
	if err := s.RevokeAPIToken(tok.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.APITokenByID(tok.ID)
	if !got.Revoked() || got.Live() {
		t.Errorf("revoked token still live: %+v", got)
	}
	first := got.RevokedAt
	_ = s.RevokeAPIToken(tok.ID)
	got, _ = s.APITokenByID(tok.ID)
	if !got.RevokedAt.Equal(first) {
		t.Errorf("re-revoking moved RevokedAt: %v -> %v", first, got.RevokedAt)
	}
	if active, _ := s.ActiveAPITokens(); len(active) != 0 {
		t.Errorf("revoked token is still active: %+v", active)
	}
	if all, _ := s.ListAPITokens(); len(all) != 1 {
		t.Errorf("revoked token dropped out of the list: %d rows", len(all))
	}
}

func TestAPITokenExpiry(t *testing.T) {
	s := open(t)
	past := &APIToken{Label: "yesterday", Hash: "h", ExpiresAt: time.Now().Add(-time.Hour)}
	future := &APIToken{Label: "tomorrow", Hash: "h", ExpiresAt: time.Now().Add(time.Hour)}
	for _, tok := range []*APIToken{past, future} {
		if err := s.CreateAPIToken(tok); err != nil {
			t.Fatal(err)
		}
	}
	active, err := s.ActiveAPITokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != future.ID {
		t.Fatalf("active set = %+v, want only the unexpired token", active)
	}
	expired, _ := s.APITokenByID(past.ID)
	if !expired.Expired() || expired.Live() {
		t.Errorf("expired token reads as live: %+v", expired)
	}
}

func TestValidScopesAndHasScope(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, DefaultAPIScope},
		{[]string{"nope"}, DefaultAPIScope},
		{[]string{"mail:send", "mail:read"}, "mail:read mail:send"},           // APIScopes order
		{[]string{"mail:read", "mail:read"}, "mail:read"},                     // deduped
		{[]string{" mail:send ", "drop table"}, "mail:send"},                  // trimmed, filtered
		{[]string{"webhooks:manage", "accounts:read"}, "accounts:read webhooks:manage"},
	}
	for _, c := range cases {
		if got := ValidScopes(c.in); got != c.want {
			t.Errorf("ValidScopes(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	tok := APIToken{Scopes: "mail:read mail:send"}
	if !tok.HasScope("mail:send") || tok.HasScope("mail:modify") || tok.HasScope("mail") {
		t.Errorf("HasScope wrong on %q", tok.Scopes)
	}
	if got := tok.ScopeList(); len(got) != 2 {
		t.Errorf("ScopeList = %q", got)
	}

	// An unknown scope cannot be stored even if the caller passes one directly.
	s := open(t)
	tok = APIToken{Label: "l", Hash: "h", Scopes: "mail:read admin:everything"}
	if err := s.CreateAPIToken(&tok); err != nil {
		t.Fatal(err)
	}
	if tok.Scopes != "mail:read" {
		t.Errorf("stored scopes = %q, want the unknown one dropped", tok.Scopes)
	}
}
