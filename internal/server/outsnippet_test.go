// SPDX-License-Identifier: AGPL-3.0-only
package server

import "testing"

func TestOutSnippet(t *testing.T) {
	cases := map[string]string{
		"<p>Hi <b>there</b></p>":      "Hi there",
		"plain &amp; simple":          "plain & simple",
		"  lots   of\n\twhitespace  ": "lots of whitespace",
		"":                            "",
	}
	for in, want := range cases {
		if got := outSnippet(in); got != want {
			t.Errorf("outSnippet(%q) = %q; want %q", in, got, want)
		}
	}
	// Long bodies are truncated with an ellipsis.
	long := ""
	for i := 0; i < 50; i++ {
		long += "word "
	}
	if got := outSnippet(long); len([]rune(got)) > 101 || got[len(got)-3:] != "…" {
		t.Errorf("long snippet not truncated: len=%d %q", len([]rune(got)), got)
	}
}
