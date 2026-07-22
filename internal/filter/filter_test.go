package filter

import (
	"encoding/json"
	"testing"
)

func TestRuleMatches(t *testing.T) {
	msg := MessageMeta{
		From:    "Alice <alice@Example.com>",
		To:      "me@example.com",
		Subject: "RE: Invoice #42",
		Body:    "Please find the invoice attached.",
	}

	tests := []struct {
		name string
		rule Rule
		want bool
	}{
		{
			name: "contains match, case-insensitive",
			rule: Rule{Conditions: []Condition{{Field: FieldFrom, Op: OpContains, Value: "EXAMPLE.COM"}}},
			want: true,
		},
		{
			name: "contains no match",
			rule: Rule{Conditions: []Condition{{Field: FieldFrom, Op: OpContains, Value: "bob@"}}},
			want: false,
		},
		{
			name: "regex match",
			rule: Rule{Conditions: []Condition{{Field: FieldSubject, Op: OpRegex, Value: `^RE:.*Invoice #\d+$`}}},
			want: true,
		},
		{
			name: "regex no match",
			rule: Rule{Conditions: []Condition{{Field: FieldSubject, Op: OpRegex, Value: `^FWD:`}}},
			want: false,
		},
		{
			name: "invalid regex never panics, treated as no match",
			rule: Rule{Conditions: []Condition{{Field: FieldSubject, Op: OpRegex, Value: `(unclosed`}}},
			want: false,
		},
		{
			name: "multiple conditions AND — all match",
			rule: Rule{Conditions: []Condition{
				{Field: FieldFrom, Op: OpContains, Value: "alice"},
				{Field: FieldSubject, Op: OpContains, Value: "invoice"},
			}},
			want: true,
		},
		{
			name: "multiple conditions AND — one fails",
			rule: Rule{Conditions: []Condition{
				{Field: FieldFrom, Op: OpContains, Value: "alice"},
				{Field: FieldSubject, Op: OpContains, Value: "receipt"},
			}},
			want: false,
		},
		{
			name: "body contains",
			rule: Rule{Conditions: []Condition{{Field: FieldBody, Op: OpContains, Value: "INVOICE ATTACHED"}}},
			want: true,
		},
		{
			name: "no conditions never matches (no accidental catch-all)",
			rule: Rule{Conditions: nil},
			want: false,
		},
		{
			name: "unknown field never matches",
			rule: Rule{Conditions: []Condition{{Field: "nonsense", Op: OpContains, Value: "x"}}},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.Matches(msg); got != tc.want {
				t.Errorf("Matches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDryRun(t *testing.T) {
	rule := Rule{Conditions: []Condition{{Field: FieldFrom, Op: OpContains, Value: "boss"}}}
	msgs := []MessageMeta{
		{From: "boss@work.com", Subject: "urgent"},
		{From: "friend@home.com", Subject: "hi"},
		{From: "the-boss@work.com", Subject: "again"},
	}
	got := DryRun(rule, msgs)
	if len(got) != 2 {
		t.Fatalf("DryRun matched %d messages, want 2: %+v", len(got), got)
	}
}

func TestRuleValidate(t *testing.T) {
	tests := []struct {
		name    string
		rule    Rule
		wantErr bool
	}{
		{"valid", Rule{Name: "x", Conditions: []Condition{{Field: FieldFrom, Op: OpContains, Value: "a"}}, Actions: []Action{{Type: ActionMarkRead}}}, false},
		{"missing name", Rule{Conditions: []Condition{{Field: FieldFrom, Op: OpContains, Value: "a"}}, Actions: []Action{{Type: ActionStar}}}, true},
		{"no conditions", Rule{Name: "x", Actions: []Action{{Type: ActionStar}}}, true},
		{"no actions", Rule{Name: "x", Conditions: []Condition{{Field: FieldFrom, Op: OpContains, Value: "a"}}}, true},
		{"bad field", Rule{Name: "x", Conditions: []Condition{{Field: "nope", Op: OpContains, Value: "a"}}, Actions: []Action{{Type: ActionStar}}}, true},
		{"bad op", Rule{Name: "x", Conditions: []Condition{{Field: FieldFrom, Op: "nope", Value: "a"}}, Actions: []Action{{Type: ActionStar}}}, true},
		{"invalid regex", Rule{Name: "x", Conditions: []Condition{{Field: FieldFrom, Op: OpRegex, Value: "(unclosed"}}, Actions: []Action{{Type: ActionStar}}}, true},
		{"bad action type", Rule{Name: "x", Conditions: []Condition{{Field: FieldFrom, Op: OpContains, Value: "a"}}, Actions: []Action{{Type: "nope"}}}, true},
		{"forward missing arg", Rule{Name: "x", Conditions: []Condition{{Field: FieldFrom, Op: OpContains, Value: "a"}}, Actions: []Action{{Type: ActionForward}}}, true},
		{"forward with arg", Rule{Name: "x", Conditions: []Condition{{Field: FieldFrom, Op: OpContains, Value: "a"}}, Actions: []Action{{Type: ActionForward, Arg: "a@b.com"}}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rule.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestRuleJSONExportShape(t *testing.T) {
	rule := Rule{
		ID: 7, Account: "work", Name: "Invoices", Position: 2, Enabled: true,
		Conditions: []Condition{{Field: FieldSubject, Op: OpContains, Value: "invoice"}},
		Actions:    []Action{{Type: ActionLabel, Arg: "Finance"}, {Type: ActionMarkRead}},
	}
	b, err := json.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "account", "name", "position", "enabled", "conditions", "actions"} {
		if _, ok := m[key]; !ok {
			t.Errorf("exported JSON missing key %q: %s", key, b)
		}
	}
	conds, ok := m["conditions"].([]any)
	if !ok || len(conds) != 1 {
		t.Fatalf("conditions shape wrong: %s", b)
	}
	cond := conds[0].(map[string]any)
	for _, key := range []string{"field", "op", "value"} {
		if _, ok := cond[key]; !ok {
			t.Errorf("condition missing key %q: %s", key, b)
		}
	}
	acts, ok := m["actions"].([]any)
	if !ok || len(acts) != 2 {
		t.Fatalf("actions shape wrong: %s", b)
	}
	// action with no Arg omits it (omitempty) — mark_read shouldn't carry "arg":""
	markRead := acts[1].(map[string]any)
	if _, ok := markRead["arg"]; ok {
		t.Errorf("action with empty arg should omit the key: %s", b)
	}

	var back Rule
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Name != rule.Name || len(back.Conditions) != 1 || len(back.Actions) != 2 {
		t.Errorf("round-trip mismatch: %+v", back)
	}
}
