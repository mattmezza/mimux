package store

import (
	"testing"

	"github.com/mattmezza/mimux/internal/config"
)

// The label column is new and sits second in accountCols; a mismatch between
// that list and scanAccount's argument order silently shifts every field.
func TestAccountLabelRoundTrip(t *testing.T) {
	s := open(t)
	if err := s.UpsertAccount(config.Account{
		Name: "GMail", Label: "Personal", Email: "a@b.c",
		Provider: "gmail", Auth: "password",
	}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAccounts()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListAccounts = %v, err = %v", list, err)
	}
	if list[0].Label != "Personal" || list[0].Email != "a@b.c" {
		t.Fatalf("got %+v", list[0])
	}
	if got := list[0].DisplayLabel(); got != "Personal" {
		t.Errorf("DisplayLabel = %q", got)
	}
}
