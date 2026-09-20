// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
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

	served, err := a.yield(context.Background(), nil)
	if err != nil {
		t.Fatalf("yield = %v", err)
	}
	if !ran {
		t.Error("yield did not run the queued command: an interactive body open would still be waiting")
	}
	if !served {
		t.Error("yield ran a command but did not report serving one: the pass it interrupted cannot know its folder needs re-selecting")
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
	if _, err := a.yield(ctx, nil); err == nil {
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

// TestFetchNoticeNamesTheBusyAccount: every surface that reports a failed fetch
// — the reading pane, the HTTP API, the MCP tools — takes its sentence from
// FetchNotice, so the busy case has to be told apart from a real network failure
// in one place rather than four.
func TestFetchNoticeNamesTheBusyAccount(t *testing.T) {
	busy := FetchNotice("body", errors.Join(ErrBusy, context.DeadlineExceeded))
	for _, want := range []string{"busy syncing", "body", "Try again"} {
		if !strings.Contains(busy, want) {
			t.Errorf("busy notice = %q, want it to contain %q", busy, want)
		}
	}
	other := FetchNotice("body", errors.New("connection refused"))
	if strings.Contains(other, "busy syncing") {
		t.Errorf("generic notice = %q, want no sync talk for a real failure", other)
	}
	if !strings.Contains(other, "offline") {
		t.Errorf("generic notice = %q, want the network story kept", other)
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

// archiveUnderInterruptedPass syncs Archive and INBOX onto an in-memory server,
// then runs one deep pass over Archive with the interactive command the mid-pass
// yield exists to serve \u2014 a read-only SELECT of the inbox, i.e. a body open
// there \u2014 queued to land on that yield. It returns the store, Archive, and what
// Archive held before the pass.
//
// The two callers differ only in which mailbox holds more messages, which is
// what decides which of the pass's two O(mailbox) stages runs against the wrong
// one.
func archiveUnderInterruptedPass(t *testing.T, archiveSubjects, inboxSubjects []string) (*store.Store, *store.Folder, map[uint32]bool) {
	t.Helper()
	st := testStore(t)
	inboxMsgs := make([]string, 0, len(inboxSubjects))
	for _, s := range inboxSubjects {
		inboxMsgs = append(inboxMsgs, testMessage("ada@example.com", s))
	}
	c, user := newTestIMAPUser(t, inboxMsgs...)
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	for _, s := range archiveSubjects {
		deliver(t, user, "Archive", testMessage("bob@example.com", s))
	}
	if _, err := a.syncFolders(c); err != nil {
		t.Fatal(err)
	}
	archive, inbox := folderByName(t, st, "acct", "Archive"), folderByName(t, st, "acct", "INBOX")
	for _, f := range []*store.Folder{archive, inbox} {
		if _, err := a.syncFolder(context.Background(), c, f, c.Caps()); err != nil {
			t.Fatal(err)
		}
	}
	before, err := st.FolderUIDs(archive.ID)
	if err != nil {
		t.Fatal(err)
	}

	// A body open in the inbox, queued while the sweep is working through
	// Archive: it SELECTs its own mailbox on the worker's connection and leaves
	// it selected.
	a.cmds <- cmd{
		fn: func(conn *imapclient.Client) error {
			_, err := conn.Select(inbox.Name, &imap.SelectOptions{ReadOnly: true}).Wait()
			return err
		},
		done: make(chan error, 1),
	}
	// Force the deep pass, as the 30-minute interval (or a restart) would.
	delete(a.sweep.deep, archive.ID)
	if _, err := a.syncFolder(context.Background(), c, archive, c.Caps()); err != nil {
		t.Fatal(err)
	}
	return st, archive, before
}

// TestYieldKeepsTheSyncedFolderSelected: a command drained mid-pass SELECTs its
// own mailbox on this connection and leaves it selected (see
// TestSelectInboxAfterReadOnlyCommand). Everything after syncFolder's yield is
// UID work on the folder being synced, so the pass has to put it back first \u2014
// otherwise it reconciles Archive against the inbox, and rows for mail that is
// still on the server are deleted (and, with announce on, announced as deleted).
func TestYieldKeepsTheSyncedFolderSelected(t *testing.T) {
	st, archive, before := archiveUnderInterruptedPass(t,
		[]string{"arch-one", "arch-two", "arch-three"}, []string{"inbox-one"})
	after, err := st.FolderUIDs(archive.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameUIDs(before, after) {
		t.Errorf("Archive's stored UIDs went %v -> %v: the pass reconciled against the mailbox the drained command selected, and nothing was deleted on the server",
			sortedUIDs(before), sortedUIDs(after))
	}
}

// TestYieldDoesNotBackfillFromTheDrainedCommandsMailbox: the mirror image.
// backfillWindow on the wrong mailbox stores that mailbox's messages as the
// synced folder's \u2014 the inbox's mail filed under Archive, announced as arrivals.
func TestYieldDoesNotBackfillFromTheDrainedCommandsMailbox(t *testing.T) {
	st, archive, before := archiveUnderInterruptedPass(t,
		[]string{"arch-one"}, []string{"inbox-one", "inbox-two", "inbox-three"})
	after, err := st.FolderUIDs(archive.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameUIDs(before, after) {
		t.Errorf("Archive's stored UIDs went %v -> %v: the pass stored the mailbox the drained command selected as Archive's",
			sortedUIDs(before), sortedUIDs(after))
	}
}

// The first reconciliation batch must give a queued read the connection, then
// restore Archive before fetching the second batch. The queued read deliberately
// selects INBOX, whose UID set differs from Archive's.
func TestDeepReconcileYieldsBetweenBatchesAndRestoresFolder(t *testing.T) {
	st, c, a, archive, inbox := largeArchiveForDeepPass(t)
	expungeOnServer(t, c, archive.Name, imap.UID(deepBatchSize+1))
	if _, err := c.Select(archive.Name, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	ran := false
	a.cmds <- cmd{fn: func(conn *imapclient.Client) error {
		ran = true
		_, err := conn.Select(inbox.Name, &imap.SelectOptions{ReadOnly: true}).Wait()
		return err
	}, done: make(chan error, 1)}
	changed, err := a.reconcileExpunged(context.Background(), c, archive, false, c.Caps().Has(imap.CapCondStore))
	if err != nil || !changed || !ran {
		t.Fatalf("reconcile: changed=%v, err=%v, command ran=%v", changed, err, ran)
	}
	got, err := st.FolderUIDs(archive.ID)
	if err != nil || len(got) != deepBatchSize || got[uint32(deepBatchSize+1)] {
		t.Fatalf("Archive UIDs after reconcile: len=%d, deleted UID present=%v, err=%v", len(got), got[uint32(deepBatchSize+1)], err)
	}
}

// Backfill searches the newest sequence window first. A queued read after that
// window must run before the next search, and the next search must use Archive.
func TestDeepBackfillYieldsBetweenSearchWindowsAndRestoresFolder(t *testing.T) {
	st, c, a, archive, inbox := largeArchiveForDeepPass(t)
	if err := st.DeleteMessageByUID(archive.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select(archive.Name, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	ran := false
	a.cmds <- cmd{fn: func(conn *imapclient.Client) error {
		ran = true
		_, err := conn.Select(inbox.Name, &imap.SelectOptions{ReadOnly: true}).Wait()
		return err
	}, done: make(chan error, 1)}
	got, err := a.backfillWindow(context.Background(), c, archive, deepBatchSize+1, c.Caps().Has(imap.CapCondStore))
	if err != nil || !ran || got != 1 {
		t.Fatalf("backfill: got=%d, err=%v, command ran=%v", got, err, ran)
	}
	if msg, _ := st.MessageByFolderUID(archive.ID, 1); msg == nil {
		t.Fatal("backfill missed the older Archive UID after the queued read changed mailboxes")
	}
}

func largeArchiveForDeepPass(t *testing.T) (*store.Store, *imapclient.Client, *account, *store.Folder, *store.Folder) {
	t.Helper()
	st := testStore(t)
	c, user := newTestIMAPUser(t, testMessage("ada@example.com", "inbox"))
	for i := 0; i < deepBatchSize+1; i++ {
		deliver(t, user, "Archive", testMessage("ada@example.com", "archive"))
	}
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	if _, err := a.syncFolders(c); err != nil {
		t.Fatal(err)
	}
	archive := folderByName(t, st, "acct", "Archive")
	inbox := folderByName(t, st, "acct", "INBOX")
	if _, err := a.syncFolder(context.Background(), c, archive, c.Caps()); err != nil {
		t.Fatal(err)
	}
	return st, c, a, archive, inbox
}

func folderByName(t *testing.T, st *store.Store, account, name string) *store.Folder {
	t.Helper()
	all, err := st.ListFolders(account)
	if err != nil {
		t.Fatal(err)
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i]
		}
	}
	t.Fatalf("no %s folder", name)
	return nil
}

func sameUIDs(a, b map[uint32]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for uid := range a {
		if !b[uid] {
			return false
		}
	}
	return true
}

func sortedUIDs(uids map[uint32]bool) []uint32 {
	out := make([]uint32, 0, len(uids))
	for uid := range uids {
		out = append(out, uid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
