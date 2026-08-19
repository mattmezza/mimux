// SPDX-License-Identifier: AGPL-3.0-only
package filter

import (
	"html/template"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// stubRuleStore is a minimal RuleStore for exercising the HTTP handlers
// without a real database.
type stubRuleStore struct {
	rules   []Rule
	recent  []MessageMeta
	created *Rule
}

func (s *stubRuleStore) ListRules() ([]Rule, error) { return s.rules, nil }
func (s *stubRuleStore) GetRule(id int64) (*Rule, error) {
	for i := range s.rules {
		if s.rules[i].ID == id {
			return &s.rules[i], nil
		}
	}
	return nil, nil
}
func (s *stubRuleStore) CreateRule(r *Rule) error       { s.created = r; return nil }
func (s *stubRuleStore) UpdateRule(r Rule) error        { return nil }
func (s *stubRuleStore) DeleteRule(id int64) error      { return nil }
func (s *stubRuleStore) ToggleRule(id int64) error      { return nil }
func (s *stubRuleStore) ReorderRules(ids []int64) error { return nil }
func (s *stubRuleStore) RecentInbox(account string, limit int) ([]MessageMeta, error) {
	return s.recent, nil
}

func testFuncs() template.FuncMap {
	return template.FuncMap{
		"folderLabel": func(f any) string { return "" },
		"acctLabel":   func(s string) string { return s },
		"relTime":     func(t time.Time) string { return "" },
		"highlight":   func(s string, terms []string) template.HTML { return template.HTML(s) }, // #nosec G203 -- test stub mirroring prod funcmap
	}
}

var testRule = Rule{
	ID: 1, Name: "Newsletters", Enabled: true,
	Conditions: []Condition{{Field: FieldFrom, Op: OpContains, Value: "newsletter"}},
	Actions:    []Action{{Type: ActionMove, Arg: "Archive"}},
}

// TestListRendersFullPage is a regression test for the blank filters page: the
// template set must cover everything filters.html reaches for (via base ->
// content), or ExecuteTemplate fails mid-stream and the client gets a
// truncated response. It also pins the rule summary, which is the whole point
// of the row: a rule reads back as the sentence its author wrote.
func TestListRendersFullPage(t *testing.T) {
	rs := &stubRuleStore{rules: []Rule{testRule}}
	r := Routes(rs, false, testFuncs(), nil)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "</html>") {
		t.Fatalf("response looks truncated, missing </html>; got:\n%s", body)
	}
	if !strings.Contains(body, `From contains &#34;newsletter&#34;`) || !strings.Contains(body, "Move to Archive") {
		t.Errorf("rule summary missing from the page; got:\n%s", body)
	}
}

// TestDryRunListsMatches: "test against recent mail" runs the same conditions
// the sync loop runs, over stored mail, and lists what would have matched. A
// read and nothing else — the non-matching message must not appear either.
func TestDryRunListsMatches(t *testing.T) {
	rs := &stubRuleStore{
		rules: []Rule{testRule},
		recent: []MessageMeta{
			{From: "newsletter@example.com", Subject: "This week's picks"},
			{From: "alice@example.com", Subject: "Lunch?"},
		},
	}
	r := Routes(rs, false, testFuncs(), nil)

	req := httptest.NewRequest("GET", "/?edit=1&test=1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "This week&#39;s picks") {
		t.Errorf("dry run did not list the matching message; got:\n%s", body)
	}
	if strings.Contains(body, "Lunch?") {
		t.Error("dry run listed a message the rule does not match")
	}
}

// TestArgLessActionDropsItsArgument: the form keeps one arg input per action
// row and only hides it, so switching "move to Archive" to "star" still posts
// the stale value. Storing it would render the rule as `Star Archive`.
func TestArgLessActionDropsItsArgument(t *testing.T) {
	rs := &stubRuleStore{}
	r := Routes(rs, false, testFuncs(), nil)

	form := url.Values{
		"name":       {"Star it"},
		"cond_field": {"from"}, "cond_op": {"contains"}, "cond_value": {"alice"},
		"act_type": {"star"}, "act_arg": {"Archive"},
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 303 {
		t.Fatalf("status = %d, want 303; body: %s", w.Code, w.Body.String())
	}
	if rs.created == nil {
		t.Fatal("no rule was created")
	}
	if arg := rs.created.Actions[0].Arg; arg != "" {
		t.Errorf("stored action arg = %q, want it dropped", arg)
	}
}
