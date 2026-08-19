// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"testing"
	"time"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

// TestUserActionsBroadcast: read, star, label and move are the four things a
// person does to a message, and until they broadcast, every other tab and
// device stayed wrong until the next sync poll. Each mutation is one new-mail.
func TestUserActionsBroadcast(t *testing.T) {
	st := testStore(t)
	c := newTestIMAP(t, testMessage("alice@example.com", "hi"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	syncInbox(t, a, c)

	folders, err := st.ListFolders("acct")
	if err != nil {
		t.Fatal(err)
	}
	var inbox, archive *store.Folder
	for i := range folders {
		switch folders[i].SpecialUse {
		case "inbox":
			inbox = &folders[i]
		case "archive":
			archive = &folders[i]
		}
	}
	if inbox == nil || archive == nil {
		t.Fatalf("folders discovered: %+v", folders)
	}
	msgs, err := st.ListMessages(inbox.ID, 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("stored %d messages: %v", len(msgs), err)
	}
	msg := &msgs[0]

	// Subscribe after the sync, so what lands here is only what the user did.
	events, unsubscribe := m.hub.subscribe()
	defer unsubscribe()
	ctx := context.Background()
	// The sync's own connection: the exported wrappers queue onto a.cmds, which
	// only a running worker drains.
	actions := []struct {
		name string
		run  func() error
	}{
		{"read", func() error { return m.setRead(ctx, c, msg, true) }},
		{"star", func() error { return m.setStarred(ctx, c, msg, true) }},
		{"label", func() error { return m.SetLabel(msg, "work", true) }},
		{"move", func() error { return m.moveTo(ctx, c, msg, archive) }},
	}
	for _, act := range actions {
		if err := act.run(); err != nil {
			t.Fatalf("%s: %v", act.name, err)
		}
		select {
		case e := <-events:
			if e.Type != "new-mail" || e.Data != "acct" {
				t.Errorf("%s broadcast %q/%q, want new-mail for the account", act.name, e.Type, e.Data)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s told nobody: another tab stays stale until the next poll", act.name)
		}
		select {
		case e := <-events:
			t.Errorf("%s broadcast twice: also %q", act.name, e.Type)
		default:
		}
	}
}
