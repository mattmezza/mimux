// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/filter"
	"github.com/mattmezza/mimux/internal/store"
)

// literal adapts a byte slice to the imap.LiteralReader the memory server's
// APPEND wants.
type literal struct{ *bytes.Reader }

func (l literal) Size() int64 { return l.Reader.Size() }

// testMessage is a minimal RFC822 message from sender with the given subject.
func testMessage(sender, subject string) string {
	return "From: <" + sender + ">\r\nTo: me@example.com\r\nSubject: " + subject +
		"\r\nDate: Mon, 02 Jan 2006 15:04:05 +0000\r\nMessage-ID: <" + subject + "@example.com>\r\n\r\nhello\r\n"
}

// newTestIMAP starts an in-memory IMAP server holding msgs in INBOX (plus an
// empty Archive and Sent) and returns a logged-in client for it — the same
// thing the worker holds while it syncs.
func newTestIMAP(t *testing.T, msgs ...string) *imapclient.Client {
	c, _ := newTestIMAPUser(t, msgs...)
	return c
}

// newTestIMAPUser is newTestIMAP plus the server-side mailbox, so a test can
// deliver mail behind the client's back — "another client appended this".
func newTestIMAPUser(t *testing.T, msgs ...string) (*imapclient.Client, *imapmemserver.User) {
	t.Helper()
	user := imapmemserver.NewUser("u", "p")
	for _, name := range []string{"INBOX", "Archive", "Sent"} {
		if err := user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range msgs {
		if _, err := user.Append("INBOX", literal{bytes.NewReader([]byte(raw))}, &imap.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mem := imapmemserver.New()
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}, imap.CapMove: {}, imap.CapUIDPlus: {}},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := imapclient.New(conn, nil)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Login("u", "p").Wait(); err != nil {
		t.Fatal(err)
	}
	return c, user
}

// syncInbox runs one full sync of the inbox with nothing draining a.cmds — the
// worker's own situation — and fails fast if it stalls instead of waiting out
// submitTimeout.
func syncInbox(t *testing.T, a *account, c *imapclient.Client) {
	t.Helper()
	folders, err := a.syncFolders(c)
	if err != nil {
		t.Fatal(err)
	}
	var inbox *store.Folder
	for i := range folders {
		if folders[i].SpecialUse == "inbox" {
			inbox = &folders[i]
		}
	}
	if inbox == nil {
		t.Fatal("no inbox discovered")
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.syncFolder(context.Background(), c, inbox, c.Caps())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sync stalled: a rule action is waiting on the worker's command queue, which only the (busy) worker drains")
	}
	if n := len(a.cmds); n != 0 {
		t.Errorf("%d rule action(s) queued on a.cmds; they must run on the sync's own client", n)
	}
}

// TestRuleActionsRunOnTheSyncConnection is the deadlock guard. runRules fires
// from inside fetchSet, i.e. on the worker goroutine — the only goroutine that
// ever drains a.cmds. An action that queued its IMAP command there would wait
// on a queue nobody can drain until it is done waiting: the sync stalls for the
// full submitTimeout and the action fails. So the actions have to run on the
// connection the sync already holds.
func TestRuleActionsRunOnTheSyncConnection(t *testing.T) {
	st := testStore(t)
	if err := st.CreateRule(&filter.Rule{
		Account: "acct", Name: "newsletters", Enabled: true,
		Conditions: []filter.Condition{{Field: filter.FieldFrom, Op: filter.OpContains, Value: "newsletter"}},
		Actions:    []filter.Action{{Type: filter.ActionMarkRead}, {Type: filter.ActionStar}},
	}); err != nil {
		t.Fatal(err)
	}

	c := newTestIMAP(t, testMessage("newsletter@example.com", "picks"))
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "syncing") // nothing drains a.cmds, exactly as during a sync
	syncInbox(t, a, c)

	// Server-side truth: both flags landed.
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	set := imap.UIDSet{}
	set.AddNum(1)
	data, err := c.Fetch(set, &imap.FetchOptions{UID: true, Flags: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1 {
		t.Fatalf("fetched %d messages, want 1", len(data))
	}
	if !hasFlag(data[0].Flags, imap.FlagSeen) {
		t.Error("mark_read action never reached the server: message is still unread")
	}
	if !hasFlag(data[0].Flags, imap.FlagFlagged) {
		t.Error("star action never reached the server: message is not flagged")
	}
}

// TestRuleMoveDuringSync is the same fix on its riskiest action: a move
// EXPUNGEs a message out of the very mailbox the sync is walking. The rest of
// the sync (backfill, expunge reconciliation) must survive that and the other
// messages in the same batch must still land.
func TestRuleMoveDuringSync(t *testing.T) {
	st := testStore(t)
	if err := st.CreateRule(&filter.Rule{
		Account: "acct", Name: "archive newsletters", Enabled: true,
		Conditions: []filter.Condition{{Field: filter.FieldFrom, Op: filter.OpContains, Value: "newsletter"}},
		Actions:    []filter.Action{{Type: filter.ActionMove, Arg: "Archive"}},
	}); err != nil {
		t.Fatal(err)
	}

	c := newTestIMAP(t,
		testMessage("newsletter@example.com", "picks"),
		testMessage("alice@example.com", "lunch"),
	)
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "syncing")
	syncInbox(t, a, c)

	inbox, err := st.FolderBySpecial("acct", "inbox")
	if err != nil || inbox == nil {
		t.Fatalf("FolderBySpecial(inbox) = %v, %v", inbox, err)
	}
	uids, err := st.FolderUIDs(inbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != 1 || !uids[2] {
		t.Errorf("stored inbox UIDs = %v, want only the unmatched message (uid 2)", uids)
	}

	if data, err := c.Select("Archive", nil).Wait(); err != nil {
		t.Fatal(err)
	} else if data.NumMessages != 1 {
		t.Errorf("Archive holds %d messages, want the moved one", data.NumMessages)
	}
	if data, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	} else if data.NumMessages != 1 {
		t.Errorf("INBOX holds %d messages, want 1 after the move", data.NumMessages)
	}
}
