// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

// eventually polls cond until it holds or the deadline passes. The steady loop
// runs on its own goroutine, so a test can only ask "has it got there yet".
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// storedUIDs is the folder's stored UID set, or nil if the folder isn't known yet.
func storedUIDs(t *testing.T, st *store.Store, account, special string) map[uint32]bool {
	t.Helper()
	f, err := st.FolderBySpecial(account, special)
	if err != nil || f == nil {
		return nil
	}
	uids, err := st.FolderUIDs(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return uids
}

// TestSteadySyncsEverySelectedFolder: mail that appears in Sent while the
// worker is connected — sent from a phone, or filed there by the server — used
// to wait for the next reconnect, because the steady state only ever re-read
// the inbox. It now lands within one cycle, and the cycle still finishes with
// the inbox selected so IDLE waits on the mailbox new mail arrives in.
func TestSteadySyncsEverySelectedFolder(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t, testMessage("ada@example.com", "hi"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "syncing")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.session(ctx, c) }()

	// The reconnect sweep has run and the loop is parked in waitWork (the memory
	// server advertises no IDLE).
	eventually(t, "the first sweep to store the inbox message", func() bool {
		return len(storedUIDs(t, st, "acct", "inbox")) == 1
	})
	eventually(t, "the account to report a completed sync", func() bool {
		return a.getStatus().State == "ok"
	})

	// Another client puts a message in Sent.
	raw := testMessage("me@example.com", "outgoing")
	if _, err := user.Append("Sent", literal{bytes.NewReader([]byte(raw))}, &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	a.signalWake()

	eventually(t, "the Sent copy to be stored within one steady cycle", func() bool {
		return len(storedUIDs(t, st, "acct", "sent")) == 1
	})
	eventually(t, "the cycle to finish", func() bool { return a.getStatus().State == "ok" })

	if mbox := c.Mailbox(); mbox == nil || mbox.Name != "INBOX" {
		t.Errorf("cycle ended on %v, want INBOX selected — IDLE waits on whatever is selected", mbox)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("session: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session did not stop on cancel")
	}
}

// TestSteadySkipsDeselectedFolder: the set is re-read every cycle, so unticking
// a folder in Settings stops costing IMAP traffic without a reconnect — and
// nothing lands in it afterwards.
func TestSteadySkipsDeselectedFolder(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t)
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "syncing")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.session(ctx, c) }()

	eventually(t, "the folders to be discovered", func() bool {
		f, _ := st.FolderBySpecial("acct", "sent")
		return f != nil && a.getStatus().State == "ok"
	})

	inbox, err := st.FolderBySpecial("acct", "inbox")
	if err != nil || inbox == nil {
		t.Fatalf("FolderBySpecial(inbox) = %v, %v", inbox, err)
	}
	if err := st.SetSyncedFolders("acct", []int64{inbox.ID}); err != nil {
		t.Fatal(err)
	}

	raw := testMessage("me@example.com", "outgoing")
	if _, err := user.Append("Sent", literal{bytes.NewReader([]byte(raw))}, &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	a.signalWake()
	eventually(t, "a cycle to run", func() bool { return a.getStatus().State == "ok" })
	// Two more, so this isn't just "the first one hadn't got there yet".
	for range 2 {
		a.signalWake()
		eventually(t, "a cycle to run", func() bool { return a.getStatus().State == "ok" })
	}
	if n := len(storedUIDs(t, st, "acct", "sent")); n != 0 {
		t.Errorf("stored %d messages in a deselected Sent, want 0", n)
	}

	cancel()
	<-done
}

// TestSelectInboxAfterReadOnlyCommand pins the hardening: a server-side search
// or an attachment listing SELECTs another mailbox on the worker's own
// connection and leaves it selected. Idling there would mean the server
// announces new mail in Archive and says nothing about the inbox.
func TestSelectInboxAfterReadOnlyCommand(t *testing.T) {
	st := testStore(t)
	c := newTestIMAP(t, testMessage("ada@example.com", "hi"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	if _, err := a.syncFolders(c); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Select("Archive", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := a.selectInbox(c); err != nil {
		t.Fatal(err)
	}
	if mbox := c.Mailbox(); mbox == nil || mbox.Name != "INBOX" {
		t.Fatalf("selected %v before idling, want INBOX", mbox)
	}
	// Idempotent: already on the inbox, nothing to do and no error.
	if err := a.selectInbox(c); err != nil {
		t.Fatal(err)
	}
}

// waitEvent returns the first event of the given type, or fails.
func waitEvent(t *testing.T, events <-chan Event, typ string) Event {
	t.Helper()
	for {
		select {
		case e := <-events:
			if e.Type == typ {
				return e
			}
		case <-time.After(time.Second):
			t.Fatalf("no %s event was published", typ)
			return Event{}
		}
	}
}

// noEvent fails if an event of the given type turns up within the window.
func noEvent(t *testing.T, events <-chan Event, typ string) {
	t.Helper()
	for {
		select {
		case e := <-events:
			if e.Type == typ {
				t.Fatalf("published %+v, wanted silence", e)
			}
		case <-time.After(200 * time.Millisecond):
			return
		}
	}
}

// TestFirstPassAnnouncesNothing: ticking a folder in Settings starts a first
// full pass on an account that has been running for hours, so the "nothing
// until the account has synced once" guard no longer covers it. Backfilling a
// year of Archive must not fire a year of notifications and webhook
// deliveries — and the very next arrival in that folder still must.
func TestFirstPassAnnouncesNothing(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t, testMessage("ada@example.com", "hi"))
	raw := testMessage("filed@example.com", "old thread")
	if _, err := user.Append("Archive", literal{bytes.NewReader([]byte(raw))}, &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	syncInbox(t, a, c)
	a.setStatus("ok", "") // the account has been up and syncing for a while

	archive, err := st.FolderByName("acct", "Archive")
	if err != nil || archive == nil {
		t.Fatalf("FolderByName(Archive) = %v, %v", archive, err)
	}

	events, unsubscribe := m.Subscribe()
	defer unsubscribe()

	// The user ticks Archive: its first pass runs.
	if _, err := a.syncFolder(context.Background(), c, archive, c.Caps()); err != nil {
		t.Fatal(err)
	}
	uids, err := st.FolderUIDs(archive.ID)
	if err != nil || len(uids) != 1 {
		t.Fatalf("the backfill stored %v (%v), want the one message", uids, err)
	}
	noEvent(t, events, "message-new")

	// The next cycle is the steady state, and an arrival there is an arrival.
	raw = testMessage("bob@example.com", "reply")
	if _, err := user.Append("Archive", literal{bytes.NewReader([]byte(raw))}, &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.syncFolder(context.Background(), c, archive, c.Caps()); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, events, "message-new")
}
