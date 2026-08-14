package filter

import (
	"html/template"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubRuleStore is a minimal RuleStore for exercising the HTTP handlers
// without a real database.
type stubRuleStore struct{}

func (stubRuleStore) ListRules() ([]Rule, error)      { return nil, nil }
func (stubRuleStore) GetRule(id int64) (*Rule, error) { return nil, nil }
func (stubRuleStore) CreateRule(r *Rule) error        { return nil }
func (stubRuleStore) UpdateRule(r Rule) error         { return nil }
func (stubRuleStore) DeleteRule(id int64) error       { return nil }
func (stubRuleStore) ToggleRule(id int64) error       { return nil }
func (stubRuleStore) ReorderRules(ids []int64) error  { return nil }

// TestListRendersFullPage is a regression test for the blank filters page:
// the "filters" template set must include every partial referenced by
// filters.html (via base -> content), notably compose_fab, or
// ExecuteTemplate fails mid-stream and the client gets a truncated/blank
// response.
func TestListRendersFullPage(t *testing.T) {
	funcs := template.FuncMap{
		"folderLabel": func(f any) string { return "" },
		"acctLabel":   func(s string) string { return s },
		"relTime":     func(t time.Time) string { return "" },
		"highlight":   func(s string, terms []string) template.HTML { return template.HTML(s) }, // #nosec G203 -- test stub mirroring prod funcmap
	}
	r := Routes(stubRuleStore{}, false, funcs, func() any { return []any{} })

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
	if !strings.Contains(body, "Filters") {
		t.Fatalf("response missing filters page content; got:\n%s", body)
	}
}
