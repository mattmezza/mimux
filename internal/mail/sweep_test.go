// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/mimux/internal/config"
)

// TestYieldDrainsTheQueue: yield is what puts a queued command on the worker's
// connection in the middle of a sweep. Nothing drains a.cmds except drain (and
// the sweeps' yield points), so a command left in the channel is a command an
// interactive request is still waiting for.
func TestYieldDrainsTheQueue(t *testing.T) {
	m := NewManager(&config.Config{}, nil)
	a := newTestAccount(m, "A", "ok")

	ran := false
	a.cmds <- cmd{fn: func(*imapclient.Client) error { ran = true; return nil }, done: make(chan error, 1)}

	if err := a.yield(context.Background(), nil); err != nil {
		t.Fatalf("yield = %v", err)
	}
	if !ran {
		t.Error("yield did not run the queued command: an interactive body open would still be waiting")
	}
}

// TestYieldStopsOnCancel: the sweeps call yield at every folder and stage
// boundary and trust it to abort the pass when the worker is being shut down —
// a cancel that only stopped at the next folder would leave the loop holding a
// connection it is about to log out of.
func TestYieldStopsOnCancel(t *testing.T) {
	m := NewManager(&config.Config{}, nil)
	a := newTestAccount(m, "A", "ok")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.yield(ctx, nil); err == nil {
		t.Error("yield returned nil on a cancelled context")
	}
}

// TestDeepPassIsIntervalBound: the whole-mailbox reconciliation is the per-cycle
// cost this issue is about, so a folder must not be able to claim one twice in a
// row — while a folder that has never had one (the reconnect catch-up, a folder
// just ticked in Settings) always gets it.
func TestDeepPassIsIntervalBound(t *testing.T) {
	m := NewManager(&config.Config{}, nil)
	a := newTestAccount(m, "A", "ok")

	if !a.deepDue(1) {
		t.Fatal("a folder that has never had a deep pass was not owed one")
	}
	a.markDeep(1)
	if a.deepDue(1) {
		t.Error("a second deep pass inside the interval: that is the O(mailbox) work on every cycle")
	}
	a.sweep.deep[1] = time.Now().Add(-2 * folderDeepPassInterval)
	if !a.deepDue(1) {
		t.Error("the interval elapsed and the folder is still not owed a deep pass")
	}
}

// TestDeepPassIsRetriedAfterAFailure: deepDue peeks and markDeep claims, so a
// pass cut short by a dropped connection is not remembered as done.
func TestDeepPassIsRetriedAfterAFailure(t *testing.T) {
	m := NewManager(&config.Config{}, nil)
	a := newTestAccount(m, "A", "ok")

	if !a.deepDue(7) {
		t.Fatal("a folder that has never had a deep pass was not owed one")
	}
	if !a.deepDue(7) {
		t.Error("a pass that never ran was treated as done")
	}
}

// TestExpungesLikely: the count signal that decides whether a cycle pays for the
// whole-mailbox UID diff. The important rows are the masked deletion — an arrival
// in the same cycle must not hide a removal, because the arrivals are accounted
// for by name — and the mailbox that grew outside our window, which is not a
// deletion at all.
func TestExpungesLikely(t *testing.T) {
	cases := []struct {
		name     string
		baseline uint32
		arrivals int
		now      uint32
		want     bool
	}{
		{"an untouched folder", 12, 0, 12, false},
		{"arrivals only", 12, 3, 15, false},
		{"a message left", 12, 0, 11, true},
		{"a removal and an arrival in the same cycle", 12, 1, 12, true},
		{"a batch left", 500, 0, 420, true},
		{"a mailbox that grew outside our window", 12, 0, 40, false},
		{"nothing known yet", 0, 0, 9, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expungesLikely(c.baseline, c.arrivals, c.now); got != c.want {
				t.Errorf("expungesLikely(%d, %d, %d) = %v, want %v", c.baseline, c.arrivals, c.now, got, c.want)
			}
		})
	}
}

// TestSweepResumesWhereTheBudgetRanOut: a sweep that stops on its budget has to
// hand the rest of the set to the next cycle, in order and exactly once each,
// and it has to say when it is done. This is the "always syncing" half of the
// issue: the sweep used to run to the end whatever it cost.
func TestSweepResumesWhereTheBudgetRanOut(t *testing.T) {
	st := testStore(t)
	c := newTestIMAP(t)
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")

	folders, err := a.syncFolders(c)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(folders))
	for i := range folders {
		ids = append(ids, folders[i].ID)
	}
	if err := st.SetSyncedFolders("acct", ids); err != nil {
		t.Fatal(err)
	}

	// A budget of zero expires between the first and second folder of the
	// rotation, so the sweep stops after exactly one and names the next one to
	// visit. (The inbox is hoisted out of the rotation and always swept first.)
	_, resume, err := a.sweepFolders(context.Background(), c, c.Caps(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if resume != "Archive" {
		t.Fatalf("resume = %q, want the folder after the one the budget covered", resume)
	}

	// Picking up there finishes the set: nothing is visited twice, and the
	// resume point comes back empty.
	_, resume, err = a.sweepFolders(context.Background(), c, c.Caps(), resume, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resume != "" {
		t.Fatalf("resume = %q, want the sweep to be complete", resume)
	}
}

// TestSweepServesAQueuedCommand is the interactive half of the issue, at the
// level the sweep can observe it: a command queued before the sweep runs is
// served by the sweep's own yield points, on the worker's connection.
func TestSweepServesAQueuedCommand(t *testing.T) {
	st := testStore(t)
	c := newTestIMAP(t)
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")

	if _, err := a.syncFolders(c); err != nil {
		t.Fatal(err)
	}

	ran := false
	a.cmds <- cmd{
		fn:   func(conn *imapclient.Client) error { ran = conn == c; return nil },
		done: make(chan error, 1),
	}
	if _, _, err := a.sweepFolders(context.Background(), c, c.Caps(), "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("the sweep never gave the queued command the worker's connection")
	}
}

// TestSyncSkipsTheWholeMailboxDiffWhenNothingLeft: the per-cycle path has to stay
// proportional to what changed. An untouched cycle keeps the baseline the server
// gave it, and a message deleted in another client still leaves the store on the
// cycle it happens.
func TestSyncSkipsTheWholeMailboxDiffWhenNothingLeft(t *testing.T) {
	st := testStore(t)
	c := newTestIMAP(t, testMessage("ada@example.com", "one"), testMessage("bob@example.com", "two"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")

	syncInbox(t, a, c)
	a.setStatus("ok", "")

	inbox, err := st.FolderBySpecial("acct", "inbox")
	if err != nil || inbox == nil {
		t.Fatalf("FolderBySpecial(inbox) = %v, %v", inbox, err)
	}
	if got := a.baselineCount(inbox.ID); got != 2 {
		t.Fatalf("baseline after the first pass = %d, want the server's 2", got)
	}

	if _, err := a.syncFolder(context.Background(), c, inbox, c.Caps()); err != nil {
		t.Fatal(err)
	}
	if got := a.baselineCount(inbox.ID); got != 2 {
		t.Errorf("baseline after an untouched cycle = %d, want 2", got)
	}

	expungeOnServer(t, c, "INBOX", 1)
	changed, err := a.syncFolder(context.Background(), c, inbox, c.Caps())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a deletion in another client did not report a change")
	}
	if got, _ := st.MessageByFolderUID(inbox.ID, 1); got != nil {
		t.Error("the deleted message is still stored")
	}
	if got := a.baselineCount(inbox.ID); got != 1 {
		t.Errorf("baseline after the deletion = %d, want 1", got)
	}
}

// TestSyncCatchesADeletionMaskedByAnArrival: the net message count is the same,
// but one message arrived and another left — the arrivals are accounted for by
// name, so the shortfall is still a deletion and the cycle still reconciles.
func TestSyncCatchesADeletionMaskedByAnArrival(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t, testMessage("ada@example.com", "one"), testMessage("bob@example.com", "two"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")

	syncInbox(t, a, c)
	a.setStatus("ok", "")

	inbox, _ := st.FolderBySpecial("acct", "inbox")
	deliver(t, user, "INBOX", testMessage("cat@example.com", "three"))
	expungeOnServer(t, c, "INBOX", 1)

	changed, err := a.syncFolder(context.Background(), c, inbox, c.Caps())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a deletion masked by an arrival reported no change")
	}
	if got, _ := st.MessageByFolderUID(inbox.ID, 1); got != nil {
		t.Error("an arrival in the same cycle hid a deletion")
	}
	uids, err := st.FolderUIDs(inbox.ID)
	if err != nil || len(uids) != 2 {
		t.Fatalf("stored UIDs = %v (%v), want the two that are still on the server", uids, err)
	}
}

// TestSyncKeepsArrivalsWhenTheDeepPassIsNotDue: the cheap path is not a cheaper
// inbox. New mail still lands, and still counts as a change worth announcing,
// between two deep passes.
func TestSyncKeepsArrivalsWhenTheDeepPassIsNotDue(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t, testMessage("ada@example.com", "one"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")

	syncInbox(t, a, c)
	a.setStatus("ok", "")

	inbox, _ := st.FolderBySpecial("acct", "inbox")
	if a.deepDue(inbox.ID) {
		t.Fatal("the first pass did not claim the deep pass: the next cycle would pay for it again")
	}

	deliver(t, user, "INBOX", testMessage("bob@example.com", "two"))
	changed, err := a.syncFolder(context.Background(), c, inbox, c.Caps())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("an arrival between two deep passes did not report a change")
	}
	uids, err := st.FolderUIDs(inbox.ID)
	if err != nil || len(uids) != 2 {
		t.Fatalf("stored UIDs after the arrival = %v (%v), want both messages", uids, err)
	}
}

// TestSyncSparesMidMoveRows: the guard on the cheap path's most dangerous
// neighbour. The row for a message that is mid-move carries the source folder's
// UID, and reconciling that folder must not read it as a deletion. The count
// signal does not change what reconcileExpunged does — it only decides when it
// runs — but the pair has to keep behaving when the count does fire.
func TestSyncSparesMidMoveRows(t *testing.T) {
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
	if err := st.SetMessageFolderPending(msg.ID, inbox.ID); err != nil {
		t.Fatal(err)
	}
	expungeOnServer(t, c, "INBOX", 2)

	if _, err := a.syncFolder(context.Background(), c, inbox, c.Caps()); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.MessageByID(msg.ID); got == nil {
		t.Fatal("the cheap path reconciled away a row that is only mid-move")
	}
}

// TestQueuedCommandReportsBusyWhenTheWorkerNeverDrains: a fetch that runs out of
// submitTimeout queued behind a sweep is not an offline account, and naming it is
// the whole point of the busy error — the reading pane renders it as a message
// instead of an iframe that would hit the same queue.
func TestQueuedCommandReportsBusyWhenTheWorkerNeverDrains(t *testing.T) {
	m := NewManager(&config.Config{}, nil)
	a := newTestAccount(m, "A", "ok")

	// Nothing drains a.cmds, so the command gives up on the caller's own bound.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := a.submitRO(ctx, func(*imapclient.Client) error { return nil })
	if err == nil {
		t.Fatal("a queued command whose worker never drains returned nil")
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("submitRO = %v, want ErrBusy", err)
	}
}

// TestCancelledRequestIsNotBusy: a request the browser abandoned must not be
// dressed up as a busy account.
func TestCancelledRequestIsNotBusy(t *testing.T) {
	m := NewManager(&config.Config{}, nil)
	a := newTestAccount(m, "A", "ok")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.submitRO(ctx, func(*imapclient.Client) error { return nil })
	if err == nil {
		t.Fatal("a command on a cancelled context returned nil")
	}
	if errors.Is(err, ErrBusy) {
		t.Fatalf("a cancelled request reported %v, want the plain context error", err)
	}
}
