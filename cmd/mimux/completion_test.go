// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"strings"
	"testing"
)

// withRegistry stubs subcommands/mailVerbs for one test and restores the real
// ones after, so the generator can be checked against a known set of verbs
// regardless of whether this test binary was built with -tags pro.
func withRegistry(t *testing.T, cmds map[string]func([]string) int, verbs []string) {
	t.Helper()
	origCmds, origVerbs := subcommands, mailVerbs
	subcommands, mailVerbs = cmds, verbs
	t.Cleanup(func() { subcommands, mailVerbs = origCmds, origVerbs })
}

func TestBashCompletionFreeBuild(t *testing.T) {
	withRegistry(t, map[string]func([]string) int{"upgrade": nil, "completion": nil}, nil)

	out := bashCompletion()
	for _, want := range []string{"upgrade", "completion", "-version", "complete -F _mimux mimux"} {
		if !strings.Contains(out, want) {
			t.Errorf("bash completion missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "mail") {
		t.Errorf("free-build registry should not surface mail:\n%s", out)
	}
}

func TestZshCompletionProBuild(t *testing.T) {
	withRegistry(t,
		map[string]func([]string) int{"upgrade": nil, "completion": nil, "mail": nil, "mcp": nil, "licence": nil},
		[]string{"login", "logout", "use", "whoami", "accounts", "folders", "list", "search", "read", "mark-read", "star", "move", "draft", "send"},
	)

	out := zshCompletion()
	for _, want := range []string{"#compdef mimux", "mail", "mcp", "licence", "upgrade", "search", "send"} {
		if !strings.Contains(out, want) {
			t.Errorf("zsh completion missing %q:\n%s", want, out)
		}
	}
}

func TestRunCompletionBadArgs(t *testing.T) {
	withRegistry(t, map[string]func([]string) int{"upgrade": nil, "completion": nil}, nil)

	for _, args := range [][]string{nil, {"fish"}, {"bash", "extra"}} {
		if code := runCompletion(args); code != 2 {
			t.Errorf("runCompletion(%v) = %d, want 2", args, code)
		}
	}
	if code := runCompletion([]string{"bash"}); code != 0 {
		t.Errorf("runCompletion([bash]) = %d, want 0", code)
	}
}
