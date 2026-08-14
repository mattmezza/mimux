package config

import "testing"

func TestDisplayLabel(t *testing.T) {
	if got := (Account{Name: "GMail"}).DisplayLabel(); got != "GMail" {
		t.Errorf("blank label = %q, want the name", got)
	}
	if got := (Account{Name: "GMail", Label: "Personal"}).DisplayLabel(); got != "Personal" {
		t.Errorf("set label = %q, want Personal", got)
	}
}
