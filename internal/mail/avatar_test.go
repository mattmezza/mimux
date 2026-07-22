package mail

import "testing"

func TestAvatarColorDeterministic(t *testing.T) {
	a := AvatarColor("alice@example.com")
	b := AvatarColor("alice@example.com")
	if a != b {
		t.Errorf("same address must map to the same color: %s vs %s", a, b)
	}
	// Case/whitespace insensitive.
	if AvatarColor("  Alice@Example.com ") != a {
		t.Error("color should ignore case and surrounding whitespace")
	}
	// Color is drawn from the palette.
	found := false
	for _, c := range avatarPalette {
		if c == a {
			found = true
		}
	}
	if !found {
		t.Errorf("color %s not in palette", a)
	}
}

func TestAvatarColorSpread(t *testing.T) {
	// Different addresses should not all collapse to one bucket.
	seen := map[string]bool{}
	for _, e := range []string{"a@x.com", "b@x.com", "c@x.com", "d@x.com", "e@x.com"} {
		seen[AvatarColor(e)] = true
	}
	if len(seen) < 2 {
		t.Errorf("expected varied colors, got %d distinct", len(seen))
	}
}

func TestAvatarInitials(t *testing.T) {
	cases := []struct{ name, email, want string }{
		{"Alice Wonderland", "a@x.com", "AW"},
		{"Bob", "b@x.com", "B"},
		{"", "carol@x.com", "C"},
		{"", "", "?"},
		{"  ", "dan@x.com", "D"},
	}
	for _, c := range cases {
		if got := AvatarInitials(c.name, c.email); got != c.want {
			t.Errorf("AvatarInitials(%q,%q) = %q, want %q", c.name, c.email, got, c.want)
		}
	}
}
