// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
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
