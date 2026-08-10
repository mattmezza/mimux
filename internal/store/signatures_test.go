package store

import "testing"

func TestSignaturesCRUDAndLink(t *testing.T) {
	s := open(t)

	if sigs, _ := s.ListSignatures(); len(sigs) != 0 {
		t.Fatalf("fresh db has %d signatures", len(sigs))
	}

	sig := &Signature{
		Account: "work", Name: "Work sig",
		TextPlain: "Matt", HTML: "<b>Matt</b>", Markdown: "**Matt**",
		ApplyMode: "first",
	}
	if err := s.UpsertSignature(sig); err != nil {
		t.Fatal(err)
	}
	if sig.ID == 0 {
		t.Fatal("insert did not set ID")
	}

	got, err := s.SignatureByID(sig.ID)
	if err != nil || got == nil {
		t.Fatalf("SignatureByID = %v, %v", got, err)
	}
	if got.HTML != "<b>Matt</b>" || got.Markdown != "**Matt**" || got.TextPlain != "Matt" || got.ApplyMode != "first" {
		t.Errorf("variants not round-tripped: %+v", got)
	}

	// Update in place.
	sig.Name = "Renamed"
	if err := s.UpsertSignature(sig); err != nil {
		t.Fatal(err)
	}
	got, _ = s.SignatureByID(sig.ID)
	if got.Name != "Renamed" {
		t.Errorf("update not persisted: %+v", got)
	}
	if all, _ := s.ListSignatures(); len(all) != 1 {
		t.Errorf("expected 1 signature after update, got %d", len(all))
	}

	// Identity link round-trip (default = none).
	if id, _ := s.IdentitySignatureID("Me@Example.com"); id != 0 {
		t.Errorf("unlinked identity should be 0, got %d", id)
	}
	if err := s.SetIdentitySignature("Me@Example.com", sig.ID); err != nil {
		t.Fatal(err)
	}
	// Address key is case-insensitive.
	if id, _ := s.IdentitySignatureID("me@example.com"); id != sig.ID {
		t.Errorf("link = %d, want %d", id, sig.ID)
	}
	m, _ := s.IdentitySignatureMap()
	if m["me@example.com"] != sig.ID {
		t.Errorf("map link = %d, want %d", m["me@example.com"], sig.ID)
	}

	// Deleting the signature cascades the link away.
	if err := s.DeleteSignature(sig.ID); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.IdentitySignatureID("me@example.com"); id != 0 {
		t.Errorf("link should be gone after signature delete, got %d", id)
	}

	// Unlink explicitly.
	sig2 := &Signature{Name: "S2", TextPlain: "x"}
	_ = s.UpsertSignature(sig2)
	_ = s.SetIdentitySignature("a@b.com", sig2.ID)
	if err := s.SetIdentitySignature("a@b.com", 0); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.IdentitySignatureID("a@b.com"); id != 0 {
		t.Errorf("unlink failed, got %d", id)
	}
}

func TestSignatureApplyRule(t *testing.T) {
	// "first message only": new/forward get it, replies do not.
	cases := []struct {
		apply, kind string
		want        bool
	}{
		{"always", "new", true},
		{"always", "reply", true},
		{"always", "reply_all", true},
		{"first", "new", true},
		{"first", "forward", true},
		{"first", "reply", false},
		{"first", "reply_all", false},
	}
	for _, c := range cases {
		if got := SignatureApplies(c.apply, c.kind); got != c.want {
			t.Errorf("SignatureApplies(%q,%q) = %v, want %v", c.apply, c.kind, got, c.want)
		}
	}
}

func TestSignatureVariant(t *testing.T) {
	sig := Signature{TextPlain: "p", HTML: "h", Markdown: "m"}
	if sig.Variant("html") != "h" || sig.Variant("markdown") != "m" || sig.Variant("plain") != "p" || sig.Variant("") != "p" {
		t.Errorf("variant selection wrong: %+v", sig)
	}
}
