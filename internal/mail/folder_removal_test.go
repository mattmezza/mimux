// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

func TestReconnectPrunesExternallyRenamedMailbox(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t)
	if err := user.Create("Example", nil); err != nil {
		t.Fatal(err)
	}
	deliver(t, user, "Example", testMessage("me@example.com", "old"))
	a := newTestAccount(NewManager(&config.Config{}, st), "acct", "syncing")
	runReconnect(t, a, c, time.Minute)
	old, err := st.FolderByName("acct", "Example")
	if err != nil || old == nil {
		t.Fatalf("original folder = %v, %v", old, err)
	}
	if msgs, err := st.ListMessages(old.ID, 10); err != nil || len(msgs) != 1 {
		t.Fatalf("original folder messages = %d, %v", len(msgs), err)
	}
	if _, err := st.UpsertFolder("another-account", "Example", "", 10); err != nil {
		t.Fatal(err)
	}
	draft := &store.Draft{Account: "acct", Body: "unfinished"}
	if err := st.UpsertDraft(draft); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearDraftDirty(draft.ID, "draft@example.com", old.ID, 1, draft.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := user.Rename("Example", "Trash/Example", nil); err != nil {
		t.Fatal(err)
	}

	runReconnect(t, a, c, time.Minute)
	if got, err := st.FolderByName("acct", "Example"); err != nil || got != nil {
		t.Fatalf("original folder after reconnect = %v, %v", got, err)
	}
	if got, err := st.ListMessages(old.ID, 10); err != nil || len(got) != 0 {
		t.Fatalf("old cached messages = %d, %v", len(got), err)
	}
	if !a.deepDue(old.ID) || a.baselineCount(old.ID) != 0 {
		t.Fatal("sweep state still refers to the removed mailbox")
	}
	if got, err := st.FolderByName("another-account", "Example"); err != nil || got == nil {
		t.Fatalf("other account's folder = %v, %v", got, err)
	}
	renamed, err := st.FolderByName("acct", "Trash/Example")
	if err != nil || renamed == nil {
		t.Fatalf("renamed folder = %v, %v", renamed, err)
	}
	if got, err := st.ListMessages(renamed.ID, 10); err != nil || len(got) != 1 {
		t.Fatalf("renamed folder messages = %d, %v", len(got), err)
	}
	got, err := st.DraftByID(draft.ID)
	if err != nil || got == nil || got.Body != "unfinished" || got.FolderID != 0 || got.UID != 0 {
		t.Fatalf("draft after removal = %+v, %v", got, err)
	}
}

func TestSweepContinuesAfterMailboxDeleted(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t)
	a := newTestAccount(NewManager(&config.Config{}, st), "acct", "syncing")
	if _, err := a.syncFolders(c); err != nil {
		t.Fatal(err)
	}
	archive, err := st.FolderByName("acct", "Archive")
	if err != nil || archive == nil {
		t.Fatalf("initial Archive = %v, %v", archive, err)
	}
	sent, err := st.FolderByName("acct", "Sent")
	if err != nil || sent == nil {
		t.Fatalf("initial Sent = %v, %v", sent, err)
	}
	if err := st.SetSyncedFolders("acct", []int64{archive.ID, sent.ID}); err != nil {
		t.Fatal(err)
	}
	if err := user.Delete("Sent"); err != nil {
		t.Fatal(err)
	}
	deliver(t, user, "Archive", testMessage("me@example.com", "still-syncing"))

	changed, resume, err := a.sweepFolders(context.Background(), c, c.Caps(), "", time.Minute)
	if err != nil || !changed || resume != "" {
		t.Fatalf("sweep = %v, %q, %v", changed, resume, err)
	}
	if got, err := st.FolderByName("acct", "Sent"); err != nil || got != nil {
		t.Fatalf("deleted Sent after sweep = %v, %v", got, err)
	}
	if got, err := st.ListMessages(archive.ID, 10); err != nil || len(got) != 1 {
		t.Fatalf("Archive messages after sweep = %d, %v", len(got), err)
	}
}

func TestIncompleteListKeepsCachedMailbox(t *testing.T) {
	st := testStore(t)
	if _, err := st.UpsertFolder("acct", "Example", "", 10); err != nil {
		t.Fatal(err)
	}
	c := scriptedIMAP(t, func(cmd string) string {
		switch {
		case strings.Contains(cmd, " CAPABILITY"):
			return "* CAPABILITY IMAP4rev1\r\n"
		case strings.Contains(cmd, " LIST "):
			return "* LIST () \"/\" INBOX\r\n* LIST \r\n"
		default:
			return ""
		}
	})
	a := newTestAccount(NewManager(&config.Config{}, st), "acct", "syncing")
	if _, err := a.syncFolders(c); err == nil {
		t.Fatal("expected the malformed LIST to fail")
	}
	if got, err := st.FolderByName("acct", "Example"); err != nil || got == nil {
		t.Fatalf("cached folder after failed LIST = %v, %v", got, err)
	}
}

func TestMissingMailboxResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		code imap.ResponseCode
		text string
		want bool
	}{
		{"response code", imap.ResponseCodeNonExistent, "Unknown Mailbox: Folder (Failure) [THROTTLED]", true},
		{"plain Dovecot response", "", "Mailbox doesn't exist: INBOX.Example (0.001 + 0.000 secs).", true},
		{"unknown mailbox", "", "Unknown Mailbox: Archive", true},
		{"no such mailbox", "", "No such mailbox", true},
		{"temporarily unavailable", imap.ResponseCodeUnavailable, "Mailbox doesn't exist: Folder", false},
		{"permission denied", "", "Permission denied", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &imap.Error{Type: imap.StatusResponseTypeNo, Code: tc.code, Text: tc.text}
			if got := isMissingMailbox(err); got != tc.want {
				t.Fatalf("isMissingMailbox(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
}
