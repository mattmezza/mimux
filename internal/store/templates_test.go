package store

import "testing"

func TestTemplatesCRUDAndOrder(t *testing.T) {
	s := open(t)

	if tpls, _ := s.ListTemplates(); len(tpls) != 0 {
		t.Fatalf("fresh db has %d templates", len(tpls))
	}

	if err := s.UpsertTemplate(&Template{Name: "   "}); err == nil {
		t.Error("blank name should be rejected")
	}

	// Insert out of order, including mixed case: listing must be alphabetical
	// case-insensitively.
	for _, name := range []string{"zeta", "Beta", "alpha"} {
		tpl := &Template{Name: name, Body: "body of " + name}
		if err := s.UpsertTemplate(tpl); err != nil {
			t.Fatal(err)
		}
		if tpl.ID == 0 {
			t.Fatal("insert did not set ID")
		}
	}
	got, err := s.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tpl := range got {
		names = append(names, tpl.Name)
	}
	if len(names) != 3 || names[0] != "alpha" || names[1] != "Beta" || names[2] != "zeta" {
		t.Errorf("not sorted alphabetically: %v", names)
	}
	if got[0].Body != "body of alpha" {
		t.Errorf("body not round-tripped: %+v", got[0])
	}

	// Update in place.
	up := got[2]
	up.Name = "aardvark"
	up.Body = "new body"
	if err := s.UpsertTemplate(&up); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListTemplates()
	if len(got) != 3 || got[0].Name != "aardvark" || got[0].Body != "new body" {
		t.Errorf("update not persisted / re-sorted: %+v", got)
	}

	if err := s.DeleteTemplate(up.ID); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.ListTemplates(); len(all) != 2 {
		t.Errorf("expected 2 templates after delete, got %d", len(all))
	}
}
