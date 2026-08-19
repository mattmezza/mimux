// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/mimux/internal/config"
)

// starOnServer flags a UID the way another IMAP client would — the change mimux
// has to notice without having made it.
func starOnServer(t *testing.T, c *imapclient.Client, folder string, uids ...imap.UID) {
	t.Helper()
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	set := imap.UIDSet{}
	set.AddNum(uids...)
	if err := c.Store(set, &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagFlagged},
	}, nil).Close(); err != nil {
		t.Fatal(err)
	}
}

// noUpdate fails if a message-updated turns up within the window.
func noUpdate(t *testing.T, events <-chan Event) {
	t.Helper()
	for {
		select {
		case e := <-events:
			if e.Type == "message-updated" {
				t.Fatalf("published %+v, wanted silence", e)
			}
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}

// TestExternalFlagChangePublishes: a star applied in another mail client shows
// up as a flag change the store did not already have, and that — and only that
// — is announced. Running the same sync again is the echo case: the store
// already holds the value, nothing moves, nothing fires.
func TestExternalFlagChangePublishes(t *testing.T) {
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
	starOnServer(t, c, "INBOX", 1)

	events, unsubscribe := m.Subscribe()
	defer unsubscribe()

	if _, err := a.fetchFlagChanges(c, inbox); err != nil {
		t.Fatal(err)
	}
	want := strconv.FormatInt(msg.ID, 10) + " starred external"
	if got := nextUpdate(t, events); got != want {
		t.Errorf("published %q, want %q", got, want)
	}
	if stored, _ := st.MessageByID(msg.ID); stored == nil || !stored.IsStarred {
		t.Errorf("the star was announced but never stored")
	}

	// The echo: same flags, store already agrees.
	if _, err := a.fetchFlagChanges(c, inbox); err != nil {
		t.Fatal(err)
	}
	noUpdate(t, events)
}

// TestExternalBurstCapSuppressesEvents: the first sync after an outage answers
// with the whole backlog at once. That is a catch-up, so the store takes every
// write and nobody is told.
func TestExternalBurstCapSuppressesEvents(t *testing.T) {
	st := testStore(t)
	c := newTestIMAP(t, testMessage("ada@example.com", "one"), testMessage("bob@example.com", "two"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	syncInbox(t, a, c)
	a.setStatus("ok", "")

	p := st.GetPrefs()
	p.ExternalBurstLimit = 1
	if err := st.SavePrefs(p); err != nil {
		t.Fatal(err)
	}

	inbox, err := st.FolderBySpecial("acct", "inbox")
	if err != nil || inbox == nil {
		t.Fatalf("FolderBySpecial(inbox) = %v, %v", inbox, err)
	}
	starOnServer(t, c, "INBOX", 1, 2)

	events, unsubscribe := m.Subscribe()
	defer unsubscribe()
	if _, err := a.fetchFlagChanges(c, inbox); err != nil {
		t.Fatal(err)
	}
	noUpdate(t, events)

	msgs, err := st.ListMessages(inbox.ID, 10)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("stored %d messages: %v", len(msgs), err)
	}
	for _, got := range msgs {
		if !got.IsStarred {
			t.Errorf("uid %d was not stored as starred: the cap must silence events, not writes", got.UID)
		}
	}
}

// TestMoveRelocatesTheRowWithTheRealUID: the local row used to keep the SOURCE
// folder's UID after a move, which meant its body could not be fetched from the
// destination and expunge reconciliation there read it as a message the server
// had dropped. COPYUID says what the UID really is; the row must take it.
func TestMoveRelocatesTheRowWithTheRealUID(t *testing.T) {
	st := testStore(t)
	c := newTestIMAP(t, testMessage("ada@example.com", "one"), testMessage("bob@example.com", "two"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	syncInbox(t, a, c)

	inbox, _ := st.FolderBySpecial("acct", "inbox")
	archive, err := st.FolderBySpecial("acct", "archive")
	if err != nil || archive == nil {
		t.Fatalf("FolderBySpecial(archive) = %v, %v", archive, err)
	}
	// UID 2 in the inbox: its destination UID must not be assumed to match.
	msg, err := st.MessageByFolderUID(inbox.ID, 2)
	if err != nil || msg == nil {
		t.Fatalf("synced message not found: %v", err)
	}
	if err := m.moveTo(context.Background(), c, msg, archive); err != nil {
		t.Fatal(err)
	}

	got, err := st.MessageByID(msg.ID)
	if err != nil || got == nil {
		t.Fatalf("the moved row is gone: %v", err)
	}
	if got.FolderID != archive.ID {
		t.Errorf("row folder = %d, want the archive (%d)", got.FolderID, archive.ID)
	}
	if got.UID != 1 {
		t.Errorf("row uid = %d, want 1 — the UID the destination assigned, not the source's", got.UID)
	}
	if _, err := st.MessageByFolderUID(inbox.ID, 2); err == nil {
		if left, _ := st.ListMessages(inbox.ID, 10); len(left) != 1 {
			t.Errorf("inbox still holds %d messages after the move", len(left))
		}
	}
}

// expungeOnServer deletes a UID the way another mail client would.
func expungeOnServer(t *testing.T, c *imapclient.Client, folder string, uid imap.UID) {
	t.Helper()
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	set := imap.UIDSet{}
	set.AddNum(uid)
	if err := c.Store(set, &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge().Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileDeletesVanishedAndAnnounces: mail deleted in another client has
// to leave mimux too. Reconciliation used to run only on a folder's first pass,
// where everything stored had just been fetched — so it never found anything and
// externally deleted mail stayed forever.
func TestReconcileDeletesVanishedAndAnnounces(t *testing.T) {
	st := testStore(t)
	c := newTestIMAP(t, testMessage("ada@example.com", "one"), testMessage("bob@example.com", "two"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	syncInbox(t, a, c)
	a.setStatus("ok", "")

	inbox, _ := st.FolderBySpecial("acct", "inbox")
	msg, err := st.MessageByFolderUID(inbox.ID, 2)
	if err != nil || msg == nil {
		t.Fatalf("synced message not found: %v", err)
	}
	expungeOnServer(t, c, "INBOX", 2)

	events, unsubscribe := m.Subscribe()
	defer unsubscribe()
	removed, err := a.reconcileExpunged(c, inbox, true)
	if err != nil || !removed {
		t.Fatalf("reconcileExpunged = %v, %v; want it to report a removal", removed, err)
	}
	if got, _ := st.MessageByID(msg.ID); got != nil {
		t.Error("the row is still here: mail deleted elsewhere never leaves mimux")
	}
	var payload map[string]any
	deadline := time.After(time.Second)
	for payload == nil {
		select {
		case e := <-events:
			if e.Type != "message-deleted" {
				continue
			}
			if err := json.Unmarshal([]byte(e.Data), &payload); err != nil {
				t.Fatalf("payload %q: %v", e.Data, err)
			}
		case <-deadline:
			t.Fatal("no message-deleted event was published")
		}
	}
	// Read before the delete: the payload has to describe a row that is gone.
	if payload["subject"] != "two" || payload["folder"] != "INBOX" {
		t.Errorf("payload = %v", payload)
	}
	if _, ok := payload["body"]; ok {
		t.Error("payload carries a body")
	}
}

// TestReconcileSpareTheRowMidMove: between the click and the deferred IMAP move
// the row sits in its destination carrying the source folder's UID. Reconciling
// that folder must not read it as a message the server dropped — it would vanish
// and then re-arrive as new mail.
func TestReconcileSparesTheRowMidMove(t *testing.T) {
	st := testStore(t)
	c := newTestIMAP(t, testMessage("ada@example.com", "one"), testMessage("bob@example.com", "two"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	syncInbox(t, a, c)
	a.setStatus("ok", "")

	inbox, _ := st.FolderBySpecial("acct", "inbox")
	msg, err := st.MessageByFolderUID(inbox.ID, 2)
	if err != nil || msg == nil {
		t.Fatalf("synced message not found: %v", err)
	}
	// The optimistic half of a move into this folder: the row is here, the UID
	// is not the server's, and the real move has not happened yet.
	if err := st.SetMessageFolderPending(msg.ID, inbox.ID); err != nil {
		t.Fatal(err)
	}
	expungeOnServer(t, c, "INBOX", 2)

	events, unsubscribe := m.Subscribe()
	defer unsubscribe()
	if _, err := a.reconcileExpunged(c, inbox, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.MessageByID(msg.ID); got == nil {
		t.Fatal("the row mid-move was reconciled away; the move would re-arrive as new mail")
	}
	select {
	case e := <-events:
		if e.Type == "message-deleted" {
			t.Errorf("published %+v for a row that is only mid-move", e)
		}
	case <-time.After(100 * time.Millisecond):
	}
}
