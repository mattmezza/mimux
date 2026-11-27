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
	"github.com/mattmezza/mimux/internal/store"
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
	selected, err := c.Select("Archive", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	storedArchive, err := st.FolderByID(archive.ID)
	if err != nil || storedArchive == nil || storedArchive.UIDValidity != selected.UIDValidity {
		t.Fatalf("stored archive = %+v (%v), want server UIDVALIDITY %d", storedArchive, err, selected.UIDValidity)
	}
	if _, err := st.MessageByFolderUID(inbox.ID, 2); err == nil {
		if left, _ := st.ListMessages(inbox.ID, 10); len(left) != 1 {
			t.Errorf("inbox still holds %d messages after the move", len(left))
		}
	}
}

func TestMoveIntoRecreatedMailboxWithReusedUID(t *testing.T) {
	for _, method := range []string{"MOVE", "COPY"} {
		t.Run(method, func(t *testing.T) {
			st := testStore(t)
			caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}}
			if method == "MOVE" {
				caps[imap.CapMove] = struct{}{}
			}
			c, user := newTestIMAPCaps(t, caps, testMessage("ada@example.com", "moving"))
			deliver(t, user, "Archive", testMessage("bob@example.com", "old"))
			m := NewManager(&config.Config{}, st)
			a := newTestAccount(m, "acct", "ok")
			syncInbox(t, a, c)
			syncSpecial(t, a, c, "archive")

			inbox, _ := st.FolderBySpecial("acct", "inbox")
			archive, _ := st.FolderBySpecial("acct", "archive")
			msg, err := st.MessageByFolderUID(inbox.ID, 1)
			if err != nil || msg == nil {
				t.Fatalf("source UID 1: %+v (%v)", msg, err)
			}
			old, err := st.MessageByFolderUID(archive.ID, 1)
			if err != nil || old == nil || old.MessageID != "old@example.com" {
				t.Fatalf("old archive UID 1: %+v (%v)", old, err)
			}
			if err := user.Delete("Archive"); err != nil {
				t.Fatal(err)
			}
			if err := user.Create("Archive", nil); err != nil {
				t.Fatal(err)
			}

			move := store.MessageMove{ID: msg.ID, SourceID: inbox.ID, TargetID: archive.ID}
			if err := st.StageMessageMoves([]store.MessageMove{move}); err != nil {
				t.Fatal(err)
			}
			drainCmds(t, a, c)
			if err := m.MovePending(t.Context(), move); err != nil {
				t.Fatal(err)
			}
			if got, err := st.MessageByFolderUID(archive.ID, 1); err != nil || got == nil || got.ID != old.ID || got.MessageID != old.MessageID {
				t.Fatalf("move overwrote the old generation's UID 1: %+v (%v)", got, err)
			}
			if got, err := st.MessageByID(msg.ID); err != nil || got != nil {
				t.Fatalf("completed move left its source row: %+v (%v)", got, err)
			}
			if selected, err := c.Select("INBOX", nil).Wait(); err != nil || selected.NumMessages != 0 {
				t.Fatalf("INBOX after move: %+v (%v), want empty", selected, err)
			}

			syncSpecial(t, a, c, "archive")
			if got, err := st.FolderByID(archive.ID); err != nil || got == nil || got.UIDValidity == archive.UIDValidity {
				t.Fatalf("sync did not record the new archive generation: %+v (%v)", got, err)
			}
			if got, err := st.MessageByFolderUID(archive.ID, 1); err != nil || got == nil || got.MessageID != msg.MessageID || got.SourceFolderID != 0 {
				t.Fatalf("sync did not discover the moved message at reused UID 1: %+v (%v)", got, err)
			}
		})
	}
}

func TestPendingMovesWithSharedSourceUID(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t, testMessage("ada@example.com", "incoming"))
	deliver(t, user, "Sent", testMessage("me@example.com", "reply"))
	deliver(t, user, "Archive", testMessage("eve@example.com", "already-archived"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	for _, special := range []string{"inbox", "sent", "archive"} {
		syncSpecial(t, a, c, special)
	}
	archive, _ := st.FolderBySpecial("acct", "archive")
	var moves []store.MessageMove
	for _, special := range []string{"inbox", "sent"} {
		folder, _ := st.FolderBySpecial("acct", special)
		msg, err := st.MessageByFolderUID(folder.ID, 1)
		if err != nil || msg == nil {
			t.Fatalf("source message in %s: %v (%v)", special, msg, err)
		}
		moves = append(moves, store.MessageMove{ID: msg.ID, SourceID: folder.ID, TargetID: archive.ID})
	}
	if err := st.StageMessageMoves(moves); err != nil {
		t.Fatal(err)
	}
	drainCmds(t, a, c)
	for i, move := range moves {
		if err := m.MovePending(context.Background(), move); err != nil {
			t.Fatal(err)
		}
		msg, err := st.MessageByID(move.ID)
		wantUID := uint32(i + 2)
		if err != nil || msg == nil || msg.SourceFolderID != 0 || msg.FolderID != archive.ID || msg.UID != wantUID {
			t.Fatalf("move %d: got %+v (%v), want archive UID %d", move.ID, msg, err, wantUID)
		}
	}
	for _, mailbox := range []string{"INBOX", "Sent"} {
		selected, err := c.Select(mailbox, nil).Wait()
		if err != nil || selected.NumMessages != 0 {
			t.Fatalf("%s still holds moved mail: %v (%v)", mailbox, selected, err)
		}
	}
	selected, err := c.Select("Archive", nil).Wait()
	if err != nil || selected.NumMessages != 3 {
		t.Fatalf("Archive: %v (%v), want three server messages", selected, err)
	}
}

func TestSyncRepairsLegacyPendingMoves(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t)
	deliver(t, user, "Archive", testMessage("ada@example.com", "confirmed"))
	deliver(t, user, "Archive", testMessage("bob@example.com", "actual"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	syncSpecial(t, a, c, "archive")
	archive, _ := st.FolderBySpecial("acct", "archive")
	confirmed, _ := st.MessageByFolderUID(archive.ID, 1)
	if _, err := st.DB.Exec(`UPDATE messages SET pending_move = 1, message_id =
		CASE WHEN uid = 2 THEN 'ghost@example.com' ELSE message_id END WHERE folder_id = ?`, archive.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(account, folder_id, uid, message_id, pending_move)
		VALUES ('acct', ?, 90, 'missing@example.com', 1)`, archive.ID); err != nil {
		t.Fatal(err)
	}
	syncSpecial(t, a, c, "archive")
	pending, err := st.LegacyPendingMessages(archive.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("remaining pending rows: %+v (%v)", pending, err)
	}
	msg, _ := st.MessageByID(confirmed.ID)
	if msg == nil || msg.MessageID != "confirmed@example.com" {
		t.Fatalf("confirmed old row lost: %+v", msg)
	}
	actual, _ := st.MessageByFolderUID(archive.ID, 2)
	if actual == nil || actual.MessageID != "actual@example.com" {
		t.Fatalf("ghost row still shadows live archive UID: %+v", actual)
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
	removed, err := a.reconcileExpunged(context.Background(), c, inbox, true, c.Caps().Has(imap.CapCondStore))
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
	c, user := newTestIMAPUser(t, testMessage("ada@example.com", "one"), testMessage("bob@example.com", "two"))
	deliver(t, user, "Archive", testMessage("carol@example.com", "archived-one"))
	deliver(t, user, "Archive", testMessage("dave@example.com", "archived-two"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	syncSpecial(t, a, c, "archive")
	syncInbox(t, a, c)
	a.setStatus("ok", "")

	inbox, _ := st.FolderBySpecial("acct", "inbox")
	archive, _ := st.FolderBySpecial("acct", "archive")
	msg, err := st.MessageByFolderUID(archive.ID, 2)
	if err != nil || msg == nil {
		t.Fatalf("synced message not found: %v", err)
	}
	// The optimistic placement shows the row in the inbox while its UID still
	// belongs to Archive.
	if err := st.StageMessageMoves([]store.MessageMove{{ID: msg.ID, SourceID: archive.ID, TargetID: inbox.ID}}); err != nil {
		t.Fatal(err)
	}
	expungeOnServer(t, c, "INBOX", 2)

	events, unsubscribe := m.Subscribe()
	defer unsubscribe()
	if _, err := a.reconcileExpunged(context.Background(), c, inbox, true, c.Caps().Has(imap.CapCondStore)); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.MessageByID(msg.ID); got == nil {
		t.Fatal("the row mid-move was reconciled away; the move would re-arrive as new mail")
	}
	select {
	case e := <-events:
		var deleted struct {
			ID int64 `json:"id"`
		}
		if e.Type != "message-deleted" || json.Unmarshal([]byte(e.Data), &deleted) != nil || deleted.ID == msg.ID {
			t.Errorf("expected deletion of the actual inbox copy, got %+v", e)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("expected a deletion event for the actual inbox copy")
	}
}
