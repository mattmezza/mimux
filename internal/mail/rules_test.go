// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"testing"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/filter"
	"github.com/mattmezza/mimux/internal/store"
)

// TestMatchingActions is the seam test: given a set of rules and a message,
// it proves the sync engine would fire the right actions, in position
// order, across every matching rule (not just the first match).
func TestMatchingActions(t *testing.T) {
	rules := []filter.Rule{
		{
			Name: "star newsletters", Enabled: true, Position: 0,
			Conditions: []filter.Condition{{Field: filter.FieldFrom, Op: filter.OpContains, Value: "newsletter"}},
			Actions:    []filter.Action{{Type: filter.ActionStar}},
		},
		{
			Name: "mark read + archive newsletters", Enabled: true, Position: 1,
			Conditions: []filter.Condition{{Field: filter.FieldFrom, Op: filter.OpContains, Value: "newsletter"}},
			Actions:    []filter.Action{{Type: filter.ActionMarkRead}, {Type: filter.ActionMove, Arg: "Archive"}},
		},
		{
			Name: "unrelated rule", Enabled: true, Position: 2,
			Conditions: []filter.Condition{{Field: filter.FieldSubject, Op: filter.OpContains, Value: "invoice"}},
			Actions:    []filter.Action{{Type: filter.ActionDelete}},
		},
	}
	meta := filter.MessageMeta{From: "newsletter@example.com", Subject: "This week's picks"}

	got := matchingActions(rules, meta)
	want := []filter.Action{
		{Type: filter.ActionStar},
		{Type: filter.ActionMarkRead},
		{Type: filter.ActionMove, Arg: "Archive"},
	}
	if len(got) != len(want) {
		t.Fatalf("matchingActions = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("action %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestMatchingActionsNoMatch(t *testing.T) {
	rules := []filter.Rule{{
		Name: "invoices", Enabled: true,
		Conditions: []filter.Condition{{Field: filter.FieldSubject, Op: filter.OpContains, Value: "invoice"}},
		Actions:    []filter.Action{{Type: filter.ActionDelete}},
	}}
	if got := matchingActions(rules, filter.MessageMeta{Subject: "hello"}); len(got) != 0 {
		t.Errorf("matchingActions = %+v, want none", got)
	}
}

// TestForwardToSelfIsRefused: a rule forwarding to the account's own address
// (or one of its aliases) is a loop with no brake — the copy arrives back in
// the inbox, the rule matches it again, and the only thing that changes is one
// more Fwd: on the subject.
func TestForwardToSelfIsRefused(t *testing.T) {
	m := NewManager(&config.Config{}, nil)
	a := newTestAccount(m, "acct", "ok")
	a.cfg.Email = "me@example.com"
	a.cfg.Aliases = []config.Alias{{Name: "Support", Email: "support@example.com"}}

	for _, addr := range []string{"me@example.com", "ME@Example.com", "Me <me@example.com>", "support@example.com"} {
		if !m.ownAddress("acct", addr) {
			t.Errorf("ownAddress(%q) = false, want true", addr)
		}
	}
	for _, addr := range []string{"", "someone@example.com", "me@example.org"} {
		if m.ownAddress("acct", addr) {
			t.Errorf("ownAddress(%q) = true, want false", addr)
		}
	}

	msg := &store.Message{Account: "acct", Subject: "Invoice"}
	err := m.applyAction(context.Background(), nil, msg, filter.Action{Type: filter.ActionForward, Arg: "me@example.com"})
	if err == nil {
		t.Error("applyAction forwarded to the account's own address")
	}
}
