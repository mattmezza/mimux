// SPDX-License-Identifier: AGPL-3.0-only
package search

import (
	"strings"
	"testing"
)

func TestBuildLocalSQL(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		scope     Scope
		account   string
		folder    int64
		wantParts []string // substrings that must appear in the WHERE
		wantArgs  []any
	}{
		{
			name:  "free text via fts + account scope",
			query: "hello", scope: ScopeAccount, account: "A",
			wantParts: []string{"messages_fts MATCH ?", "account = ?"},
			wantArgs:  []any{`"hello"`, "A"},
		},
		{
			name:  "from expands to two likes",
			query: "from:alice", scope: ScopeAll,
			wantParts: []string{"(from_address LIKE ? OR from_name LIKE ?)"},
			wantArgs:  []any{"%alice%", "%alice%"},
		},
		{
			name:  "is unread and has attachment",
			query: "is:unread has:attachment", scope: ScopeAll,
			wantParts: []string{"is_read = 0", "has_attachment = 1"},
			wantArgs:  nil,
		},
		{
			name:  "negated from",
			query: "-from:noreply", scope: ScopeAll,
			wantParts: []string{"NOT (from_address LIKE ? OR from_name LIKE ?)"},
			wantArgs:  []any{"%noreply%", "%noreply%"},
		},
		{
			name:  "negated subject uses NOT LIKE not fts",
			query: "-subject:spam", scope: ScopeAll,
			wantParts: []string{"subject NOT LIKE ?"},
			wantArgs:  []any{"%spam%"},
		},
		{
			name:  "subject fts column filter",
			query: "subject:report", scope: ScopeAll,
			wantParts: []string{`messages_fts MATCH ?`},
			wantArgs:  []any{`subject:"report"`},
		},
		{
			name:  "has link snippet scan",
			query: "has:link", scope: ScopeAll,
			wantParts: []string{"snippet LIKE ?"},
			wantArgs:  []any{"%http%"},
		},
		{
			name:  "folder scope",
			query: "hi", scope: ScopeFolder, folder: 7,
			wantParts: []string{"folder_id = ?"},
		},
		{
			name:  "in operator subquery",
			query: "in:archive", scope: ScopeAll,
			wantParts: []string{"folder_id IN (SELECT id FROM folders WHERE lower(special_use) = ? OR name LIKE ?)"},
			wantArgs:  []any{"archive", "%archive%"},
		},
		{
			name:  "after date",
			query: "after:2026-01-01", scope: ScopeAll,
			wantParts: []string{"date >= ?"},
			wantArgs:  []any{"2026-01-01T00:00:00Z"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			where, args := BuildLocalSQL(Parse(tc.query), tc.scope, tc.account, tc.folder)
			for _, p := range tc.wantParts {
				if !strings.Contains(where, p) {
					t.Errorf("WHERE %q missing %q", where, p)
				}
			}
			if tc.wantArgs != nil {
				if len(args) != len(tc.wantArgs) {
					t.Fatalf("args = %v, want %v", args, tc.wantArgs)
				}
				for i := range args {
					if args[i] != tc.wantArgs[i] {
						t.Errorf("arg %d = %v, want %v", i, args[i], tc.wantArgs[i])
					}
				}
			}
		})
	}
}

func TestBuildLocalSQLSizes(t *testing.T) {
	where, args := BuildLocalSQL(Parse("larger:5mb"), ScopeAll, "", 0)
	if !strings.Contains(where, "size > ?") {
		t.Fatalf("missing size predicate: %q", where)
	}
	if len(args) != 1 || args[0].(int64) != 5<<20 {
		t.Fatalf("args = %v", args)
	}
}

func TestBuildLocalSQLEmpty(t *testing.T) {
	where, args := BuildLocalSQL(Parse(""), ScopeAll, "", 0)
	if where != "1=1" || args != nil {
		t.Fatalf("empty query = %q, %v", where, args)
	}
}
