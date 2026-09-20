// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/mimux/internal/config"
)

// runReconnect stops after the first completed cycle. Reading sweep state only
// after the worker exits keeps these tests independent of its scheduling.
func runReconnect(t *testing.T, a *account, c *imapclient.Client, budget time.Duration) {
	t.Helper()
	a.setStatus("syncing", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.sessionWithBudget(ctx, c, budget) }()
	eventually(t, "reconnect to report ok", func() bool { return a.getStatus().State == "ok" })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("session: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session did not stop")
	}
}

func TestReconnectFirstSyncReportsOKBeforeAllFolders(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t, testMessage("ada@example.com", "inbox"))
	for i := range 40 {
		if err := user.Create(fmt.Sprintf("Label-%02d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	a := newTestAccount(NewManager(&config.Config{}, st), "acct", "syncing")
	runReconnect(t, a, c, 0)
	if !a.sweep.reconnect || a.sweep.resume == "" {
		t.Fatalf("40-folder reconnect finished before reporting ok: %+v", a.sweep)
	}
	if got := len(storedUIDs(t, st, "acct", "inbox")); got != 1 {
		t.Fatalf("inbox messages = %d, want 1 before reporting ok", got)
	}
	if len(a.sweep.deep) >= 43 {
		t.Fatal("all folders were deep-passed in the first budgeted cycle")
	}
	// A later connection finishes the debt, including folders outside the
	// steady-state synced set.
	runReconnect(t, a, c, time.Minute)
	if a.sweep.reconnect || a.sweep.resume != "" || len(a.sweep.deep) != 43 {
		t.Fatalf("reconnect debt was not completed: resume=%q deep=%d", a.sweep.resume, len(a.sweep.deep))
	}
}

func TestReconnectResumesInSameSession(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t, testMessage("ada@example.com", "inbox"))
	if _, err := user.Append("Archive", literal{bytes.NewReader([]byte(testMessage("bob@example.com", "archive")))}, &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	a := newTestAccount(NewManager(&config.Config{}, st), "acct", "syncing")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.sessionWithBudget(ctx, c, 0) }()
	eventually(t, "first reconnect cycle to report ok", func() bool { return a.getStatus().State == "ok" })
	if got := len(storedUIDs(t, st, "acct", "inbox")); got != 1 {
		t.Fatalf("inbox messages = %d before resume", got)
	}
	a.signalWake() // advance the pending pass without waiting five seconds
	eventually(t, "Archive to be synced by resumed cycle", func() bool {
		f, err := st.FolderByName("acct", "Archive")
		if err != nil || f == nil {
			return false
		}
		uids, err := st.FolderUIDs(f.ID)
		return err == nil && len(uids) == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReconnectShortAndExpiredDeepPass(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t)
	if _, err := user.Append("Archive", literal{bytes.NewReader([]byte(testMessage("ada@example.com", "old")))}, &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	a := newTestAccount(NewManager(&config.Config{}, st), "acct", "syncing")
	runReconnect(t, a, c, time.Minute)
	archive, err := st.FolderByName("acct", "Archive")
	if err != nil || archive == nil {
		t.Fatalf("Archive = %v, %v", archive, err)
	}
	first := a.sweep.deep[archive.ID]
	runReconnect(t, a, c, time.Minute)
	if got := a.sweep.deep[archive.ID]; !got.Equal(first) {
		t.Fatalf("short reconnect repeated Archive deep pass: %v -> %v", first, got)
	}
	for id := range a.sweep.deep {
		a.sweep.deep[id] = time.Now().Add(-2 * folderDeepPassInterval)
	}
	runReconnect(t, a, c, 0)
	if !a.sweep.reconnect || a.sweep.resume == "" {
		t.Fatal("expired deep passes did not leave resumable work")
	}
	if !a.deepDue(archive.ID) {
		t.Fatal("Archive deep pass was claimed before the resumed cycle reached it")
	}
	runReconnect(t, a, c, time.Minute)
	if a.deepDue(archive.ID) || a.sweep.reconnect {
		t.Fatal("expired deep passes were not completed after resuming")
	}
}

func TestReconnectAfterProcessRestart(t *testing.T) {
	st := testStore(t)
	c := newTestIMAP(t)
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "syncing")
	runReconnect(t, a, c, time.Minute)
	// The new account has no in-memory timestamps, as after a process restart.
	restarted := newTestAccount(m, "acct", "syncing")
	runReconnect(t, restarted, c, 0)
	if !restarted.sweep.reconnect || restarted.sweep.resume == "" {
		t.Fatal("restart did not bound and retain the reconnect work")
	}
	if len(restarted.sweep.deep) >= 3 {
		t.Fatal("restart deep-passed every folder before reporting ok")
	}
	runReconnect(t, restarted, c, time.Minute)
	if restarted.sweep.reconnect || len(restarted.sweep.deep) != 3 {
		t.Fatal("restart did not eventually deep-pass every folder")
	}
	if fs, err := st.ListFolders("acct"); err != nil || len(fs) != 3 {
		t.Fatalf("stored folders after restart = %d, %v", len(fs), err)
	}
}
