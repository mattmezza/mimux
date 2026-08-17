// SPDX-License-Identifier: AGPL-3.0-only
package server

import "testing"

// TestComposeFragment: the typeahead must query only the token being typed —
// earlier, already-accepted addresses in the field are not part of the query.
func TestComposeFragment(t *testing.T) {
	cases := map[string]string{
		"":                            "",
		"al":                          "al",
		"  al  ":                      "al",
		"bob@x.com, al":               "al",
		"bob@x.com,al":                "al",
		"Bob <bob@x.com>, ":           "",
		"Bob <bob@x.com>, Alice <ali": "ali",
		"Alice <ali>":                 "ali",
		"a@x.com, b@x.com, c":         "c",
	}
	for in, want := range cases {
		if got := composeFragment(in); got != want {
			t.Errorf("composeFragment(%q) = %q; want %q", in, got, want)
		}
	}
}
