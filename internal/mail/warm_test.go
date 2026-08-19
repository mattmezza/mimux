// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"testing"
)

// TestWarmPassCachesDraftBodies: the warmer fills the Drafts folder as well as
// the inbox, so opening — or adopting — a draft written on another device does
// not pay a cold BODY[] fetch. Same cache size, same prune, one more SELECT.
func TestWarmPassCachesDraftBodies(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t, "From: a@example.com\r\nSubject: inbox one\r\n\r\nhello\r\n")
	if err := user.Create("Drafts", nil); err != nil {
		t.Fatal(err)
	}
	a := draftAccount(t, st, c)
	draft := foreignDraft(t, a, c, user, ComposeInput{
		To: []string{"ada@example.com"}, Subject: "warm me", Body: "half a thought",
		MessageID: "cold@example.com",
	})
	inbox, err := st.FolderBySpecial("acct", "inbox")
	if err != nil || inbox == nil {
		t.Fatalf("FolderBySpecial(inbox) = %v, %v", inbox, err)
	}
	if _, err := a.syncFolder(context.Background(), c, inbox, c.Caps()); err != nil {
		t.Fatal(err)
	}
	inboxMsgs, err := st.ListMessages(inbox.ID, 10)
	if err != nil || len(inboxMsgs) != 1 {
		t.Fatalf("ListMessages(inbox) = %v, %v", inboxMsgs, err)
	}

	if err := a.warmPass(context.Background(), c); err != nil {
		t.Fatalf("warmPass: %v", err)
	}
	for _, m := range []struct {
		what string
		id   int64
	}{{"inbox", inboxMsgs[0].ID}, {"draft", draft.ID}} {
		if _, ok, err := st.GetMessageBody(m.id); err != nil || !ok {
			t.Errorf("%s body cached = %v, %v, want it warmed", m.what, ok, err)
		}
	}
}
