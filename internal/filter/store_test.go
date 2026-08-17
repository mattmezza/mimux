// SPDX-License-Identifier: AGPL-3.0-only
// Package filter_test exercises the store's filter-rule CRUD against a real
// (temp file) SQLite db. It lives here rather than internal/store because
// store.go imports filter for the Rule type — an external test package
// (filter_test) can import both without an import cycle.
package filter_test

import (
	"path/filepath"
	"testing"

	"github.com/mattmezza/mimux/internal/filter"
	"github.com/mattmezza/mimux/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestFilterRuleCRUD(t *testing.T) {
	s := openStore(t)

	if rules, err := s.ListRules(); err != nil || len(rules) != 0 {
		t.Fatalf("fresh db ListRules = %v, %v", rules, err)
	}

	r := filter.Rule{
		Account: "work",
		Name:    "Invoices",
		Enabled: true,
		Conditions: []filter.Condition{
			{Field: filter.FieldSubject, Op: filter.OpContains, Value: "invoice"},
		},
		Actions: []filter.Action{
			{Type: filter.ActionLabel, Arg: "Finance"},
			{Type: filter.ActionMarkRead},
		},
	}
	if err := s.CreateRule(&r); err != nil {
		t.Fatal(err)
	}
	if r.ID == 0 {
		t.Fatal("CreateRule did not assign an id")
	}

	got, err := s.GetRule(r.ID)
	if err != nil || got == nil {
		t.Fatalf("GetRule = %v, %v", got, err)
	}
	if got.Name != r.Name || got.Account != r.Account || len(got.Conditions) != 1 || len(got.Actions) != 2 {
		t.Fatalf("GetRule round-trip mismatch: %+v", got)
	}
	if got.Actions[1].Type != filter.ActionMarkRead || got.Actions[1].Arg != "" {
		t.Fatalf("action round-trip mismatch: %+v", got.Actions[1])
	}

	// second rule to exercise ordering / RulesForAccount scoping
	global := filter.Rule{
		Name: "Spam", Enabled: true,
		Conditions: []filter.Condition{{Field: filter.FieldFrom, Op: filter.OpContains, Value: "spam"}},
		Actions:    []filter.Action{{Type: filter.ActionDelete}},
	}
	if err := s.CreateRule(&global); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListRules()
	if err != nil || len(all) != 2 {
		t.Fatalf("ListRules = %v, %v", all, err)
	}
	if all[0].ID != r.ID || all[1].ID != global.ID {
		t.Fatalf("ListRules not in position order: %+v", all)
	}

	forWork, err := s.RulesForAccount("work")
	if err != nil || len(forWork) != 2 { // scoped + global
		t.Fatalf("RulesForAccount(work) = %v, %v", forWork, err)
	}
	forOther, err := s.RulesForAccount("personal")
	if err != nil || len(forOther) != 1 { // global only
		t.Fatalf("RulesForAccount(personal) = %v, %v", forOther, err)
	}

	// update
	got.Name = "Invoices (updated)"
	got.Enabled = false
	if err := s.UpdateRule(*got); err != nil {
		t.Fatal(err)
	}
	updated, err := s.GetRule(r.ID)
	if err != nil || updated == nil || updated.Name != "Invoices (updated)" || updated.Enabled {
		t.Fatalf("UpdateRule did not persist: %+v, %v", updated, err)
	}

	// toggle flips enabled back
	if err := s.ToggleRule(r.ID); err != nil {
		t.Fatal(err)
	}
	if toggled, _ := s.GetRule(r.ID); !toggled.Enabled {
		t.Fatalf("ToggleRule did not re-enable: %+v", toggled)
	}

	// reorder: put global first
	if err := s.ReorderRules([]int64{global.ID, r.ID}); err != nil {
		t.Fatal(err)
	}
	reordered, err := s.ListRules()
	if err != nil || len(reordered) != 2 || reordered[0].ID != global.ID || reordered[1].ID != r.ID {
		t.Fatalf("ReorderRules did not apply: %+v, %v", reordered, err)
	}

	// delete
	if err := s.DeleteRule(r.ID); err != nil {
		t.Fatal(err)
	}
	if left, err := s.ListRules(); err != nil || len(left) != 1 || left[0].ID != global.ID {
		t.Fatalf("DeleteRule did not remove the rule: %+v, %v", left, err)
	}
	if missing, err := s.GetRule(r.ID); err != nil || missing != nil {
		t.Fatalf("GetRule on deleted id = %v, %v", missing, err)
	}
}
