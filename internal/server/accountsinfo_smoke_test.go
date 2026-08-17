// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

func TestAccountsInfoSmoke(t *testing.T) {
	s := serverWith(t, []config.Account{{Name: "a", Email: "a@x.com", Auth: "password"}}, func(st *store.Store) {
		// accountViews() reads the accounts table (via refreshAccounts), and
		// ListAccounts drops anything NormalizeAccount rejects — so seed a
		// complete account, not a name-only stub.
		if err := st.UpsertAccount(config.Account{
			Name: "a", Email: "a@x.com", Provider: "gmail", Auth: "password", Password: "pw",
		}); err != nil {
			t.Fatal(err)
		}
		f, _ := st.UpsertFolder("a", "INBOX", "inbox", 0)
		_ = st.UpsertMessage(&store.Message{Account: "a", FolderID: f, UID: 1, Size: 2048})
		_ = st.RecountUnread(f)
	})
	w := httptest.NewRecorder()
	s.handleAccountsInfo(w, httptest.NewRequest("GET", "/accounts/info", nil))
	body := w.Body.String()
	for _, want := range []string{"a@x.com", "Mail size", "2 KB", "Database on disk", "Sync now"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}
