// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"strconv"
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

// nextUpdate returns the next message-updated event's data, or fails.
func nextUpdate(t *testing.T, events <-chan Event) string {
	t.Helper()
	for {
		select {
		case e := <-events:
			if e.Type == "message-updated" {
				return e.Data
			}
		case <-time.After(time.Second):
			t.Fatal("no message-updated event was published")
			return ""
		}
	}
}

// TestMutationsPublishMessageUpdated: each of the four mutation helpers
// announces what it changed, by store id, once the change has actually landed.
func TestMutationsPublishMessageUpdated(t *testing.T) {
	st := testStore(t)
	c := newTestIMAP(t, testMessage("ada@example.com", "hi"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	syncInbox(t, a, c)
	a.setStatus("ok", "") // stamps LastSync: the backfill is over

	inbox, err := st.FolderBySpecial("acct", "inbox")
	if err != nil || inbox == nil {
		t.Fatalf("FolderBySpecial(inbox) = %v, %v", inbox, err)
	}
	msg, err := st.MessageByFolderUID(inbox.ID, 1)
	if err != nil || msg == nil {
		t.Fatalf("synced message not found: %v", err)
	}

	events, unsubscribe := m.Subscribe()
	defer unsubscribe()
	ctx := context.Background()
	id := strconv.FormatInt(msg.ID, 10)

	// Each runs on the sync's own connection, so nothing waits on a.cmds.
	for _, step := range []struct {
		name string
		do   func() error
		want string
	}{
		{"star", func() error { return m.setStarred(ctx, c, msg, true) }, id + " starred mimux"},
		{"unstar", func() error { return m.setStarred(ctx, c, msg, false) }, id + " unstarred mimux"},
		{"read", func() error { return m.setRead(ctx, c, msg, true) }, id + " read mimux"},
		{"label", func() error { return m.SetLabel(msg, "work", true) }, id + " labeled mimux"},
		{"unlabel", func() error { return m.SetLabel(msg, "work", false) }, id + " unlabeled mimux"},
		{"move", func() error { return m.moveToFolder(ctx, c, msg, "Archive") }, id + " moved mimux"},
	} {
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := nextUpdate(t, events); got != step.want {
			t.Errorf("%s published %q, want %q", step.name, got, step.want)
		}
	}
}

// TestMessageUpdatedSilentDuringBackfill is the storm guard: filter rules drive
// these same helpers from inside the sync loop, once per newly-stored message,
// so a first sync must announce nothing.
func TestMessageUpdatedSilentDuringBackfill(t *testing.T) {
	st := testStore(t)
	m := NewManager(&config.Config{}, st)
	newTestAccount(m, "acct", "syncing") // LastSync zero: still backfilling

	fid, err := st.UpsertFolder("acct", "INBOX", "inbox", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMessage(&store.Message{
		Account: "acct", FolderID: fid, UID: 1, Subject: "old", Date: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	msg, err := st.MessageByFolderUID(fid, 1)
	if err != nil || msg == nil {
		t.Fatalf("seeded message not found: %v", err)
	}

	events, unsubscribe := m.Subscribe()
	defer unsubscribe()
	if err := m.SetLabel(msg, "work", true); err != nil {
		t.Fatal(err)
	}
	// new-mail is fine and expected — it is "the list moved, re-read it", which
	// costs an open tab one refresh. message-updated is a webhook delivery per
	// message, which is what must stay silent until the backfill is over.
	for {
		select {
		case e := <-events:
			if e.Type == "message-updated" {
				t.Fatalf("the backfill published %+v", e)
			}
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}
