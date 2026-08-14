package config

import (
	"encoding/json"
	"testing"
)

func TestDisplayLabel(t *testing.T) {
	if got := (Account{Name: "GMail"}).DisplayLabel(); got != "GMail" {
		t.Errorf("blank label = %q, want the name", got)
	}
	if got := (Account{Name: "GMail", Label: "Personal"}).DisplayLabel(); got != "Personal" {
		t.Errorf("set label = %q, want Personal", got)
	}
}

// Aliases ride to the edit form as JSON and the prefill reads lowercase keys;
// rows written before the tags used Name/Email and must still decode.
func TestAliasJSONKeys(t *testing.T) {
	b, err := json.Marshal([]Alias{{Name: "Sales", Email: "s@x.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `[{"name":"Sales","email":"s@x.com"}]` {
		t.Errorf("marshal = %s", got)
	}
	var old []Alias
	if err := json.Unmarshal([]byte(`[{"Name":"Sales","Email":"s@x.com"}]`), &old); err != nil {
		t.Fatal(err)
	}
	if len(old) != 1 || old[0].Name != "Sales" || old[0].Email != "s@x.com" {
		t.Errorf("legacy decode = %+v", old)
	}
}
