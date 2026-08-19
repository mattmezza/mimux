// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattmezza/mimux/internal/store"
)

// ntfyCapture points the manager's ntfy transport at a local listener and hands
// back the channel every notification lands on. ntfy is the transport with no
// key exchange, so it is the cheap way to observe what the notifier decided.
func ntfyCapture(t *testing.T, m *Manager) chan [3]string {
	t.Helper()
	got := make(chan [3]string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- [3]string{r.Header.Get("Title"), string(b), r.Header.Get("Click")}
	}))
	t.Cleanup(srv.Close)
	prefs := m.st.GetPrefs()
	prefs.NtfyURL = srv.URL
	prefs.NotifyScope = "all"
	if err := m.st.SavePrefs(prefs); err != nil {
		t.Fatal(err)
	}
	return got
}

// seedInbox stores a message in a folder and returns its id.
func seedInbox(t *testing.T, m *Manager, folder int64, account string, uid uint32, from, subject string, read bool) int64 {
	t.Helper()
	msg := &store.Message{
		Account: account, FolderID: folder, UID: uid, FromName: from, FromAddress: "x@example.test",
		Subject: subject, Date: time.Now().Add(-time.Duration(uid) * time.Minute), IsRead: read,
	}
	if err := m.st.UpsertMessage(msg); err != nil {
		t.Fatal(err)
	}
	got, err := m.st.MessageByFolderUID(folder, uid)
	if err != nil || got == nil {
		t.Fatalf("seeded message not found: %v", err)
	}
	return got.ID
}

// startNotifier runs the loop over a subscription the test owns, so a broadcast
// is guaranteed to be seen (the hub does not replay to a late subscriber).
func startNotifier(t *testing.T, m *Manager) {
	t.Helper()
	events, unsubscribe := m.hub.subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); unsubscribe() })
	go m.notifyLoop(ctx, events, 25*time.Millisecond)
}

func signal(m *Manager, ids ...int64) {
	for _, id := range ids {
		m.hub.broadcast(Event{Type: "message-new", Data: strconv.FormatInt(id, 10)})
	}
}

// TestNotifierBatchesOneWindow: five messages in one sync cycle is one
// notification, not five. This is the entire reason the trigger moved off the
// sync loop.
func TestNotifierBatchesOneWindow(t *testing.T) {
	m := testManager(t)
	m.cfg.Server.BaseURL = "https://mail.example.com"
	got := ntfyCapture(t, m)
	inbox, _ := m.st.UpsertFolder("A", "INBOX", "inbox", 0)
	var ids []int64
	for i := 1; i <= 5; i++ {
		ids = append(ids, seedInbox(t, m, inbox, "A", uint32(i), "Sender"+strconv.Itoa(i), "Subject"+strconv.Itoa(i), false))
	}
	startNotifier(t, m)
	signal(m, ids...)

	n := <-waitNotify(t, got)
	if n[0] != "5 new messages" {
		t.Errorf("Title = %q, want the batch headline", n[0])
	}
	// The body earns its place: it names who wrote, then says how many are left.
	if !strings.Contains(n[1], "Sender1 — Subject1") || !strings.Contains(n[1], "and 2 more") {
		t.Errorf("body = %q", n[1])
	}
	// A batch has no one message to open, so it opens All inboxes.
	if n[2] != "https://mail.example.com/" {
		t.Errorf("Click = %q, want the All inboxes screen", n[2])
	}
	assertQuiet(t, got)
}

// A single message keeps the old shape: who it is from, what it says, and a tap
// that opens that message.
func TestNotifierSingleMessageDeepLinks(t *testing.T) {
	m := testManager(t)
	m.cfg.Server.BaseURL = "https://mail.example.com"
	got := ntfyCapture(t, m)
	inbox, _ := m.st.UpsertFolder("A", "INBOX", "inbox", 0)
	id := seedInbox(t, m, inbox, "A", 1, "Alice", "Lunch?", false)
	startNotifier(t, m)
	// Twice: the same message signalled twice is still one notification.
	signal(m, id, id)

	n := <-waitNotify(t, got)
	if n[0] != "Alice · A" || n[1] != "Lunch?" {
		t.Errorf("notification = %q / %q", n[0], n[1])
	}
	if n[2] != messageLink("https://mail.example.com", inbox, id) {
		t.Errorf("Click = %q", n[2])
	}
	assertQuiet(t, got)
}

// The guards that moved out of notifiable(): the Sent copy of your own mail and
// anything already read must not buzz. Two accounts in one window is still one
// notification — the batch is general, not per account.
func TestNotifierGuardsAndCrossAccountBatch(t *testing.T) {
	m := testManager(t)
	got := ntfyCapture(t, m)
	inboxA, _ := m.st.UpsertFolder("A", "INBOX", "inbox", 0)
	inboxB, _ := m.st.UpsertFolder("B", "INBOX", "inbox", 0)
	sent, _ := m.st.UpsertFolder("A", "Sent", "sent", 1)
	a := seedInbox(t, m, inboxA, "A", 1, "Alice", "Hi", false)
	b := seedInbox(t, m, inboxB, "B", 1, "Bob", "Yo", false)
	ignored := []int64{
		seedInbox(t, m, sent, "A", 2, "Me", "My own mail", false),
		seedInbox(t, m, inboxA, "A", 3, "Carol", "Read elsewhere", true),
	}
	startNotifier(t, m)
	signal(m, append([]int64{a, b}, ignored...)...)

	n := <-waitNotify(t, got)
	if n[0] != "2 new messages" {
		t.Errorf("Title = %q: the Sent copy or the read message got through", n[0])
	}
	assertQuiet(t, got)
}

// The master switch is off by default and the notifier must honour it: no
// prefs change, no buzz, however much mail arrives.
func TestNotifierRespectsScopeOff(t *testing.T) {
	m := testManager(t)
	got := ntfyCapture(t, m)
	prefs := m.st.GetPrefs()
	prefs.NotifyScope = "off"
	if err := m.st.SavePrefs(prefs); err != nil {
		t.Fatal(err)
	}
	inbox, _ := m.st.UpsertFolder("A", "INBOX", "inbox", 0)
	id := seedInbox(t, m, inbox, "A", 1, "Alice", "Lunch?", false)
	startNotifier(t, m)
	signal(m, id)
	assertQuiet(t, got)
}

func waitNotify(t *testing.T, got chan [3]string) chan [3]string {
	t.Helper()
	out := make(chan [3]string, 1)
	go func() {
		select {
		case n := <-got:
			out <- n
		case <-time.After(5 * time.Second):
			t.Error("no notification arrived")
			close(out)
		}
	}()
	return out
}

// assertQuiet fails if a second notification shows up: the batch is one buzz.
func assertQuiet(t *testing.T, got chan [3]string) {
	t.Helper()
	select {
	case n := <-got:
		t.Errorf("a second notification was sent: %q / %q", n[0], n[1])
	case <-time.After(150 * time.Millisecond):
	}
}

// TestOnlyInboxMailBuzzes is a regression pin, not a new rule. The steady loop
// now re-reads Sent, Drafts and whatever else the account selected on every
// cycle, so mail lands in those folders within a minute of being written
// elsewhere instead of on the next reconnect. The one thing standing between
// that and your phone buzzing every time you send something from your laptop is
// flushNotify's inbox-only filter. Nothing here fires: not the Sent copy, not
// the draft, not the filed archive copy.
func TestOnlyInboxMailBuzzes(t *testing.T) {
	m := testManager(t)
	got := ntfyCapture(t, m)
	if _, err := m.st.UpsertFolder("A", "INBOX", "inbox", 0); err != nil {
		t.Fatal(err)
	}
	sent, _ := m.st.UpsertFolder("A", "Sent", "sent", 1)
	drafts, _ := m.st.UpsertFolder("A", "Drafts", "drafts", 2)
	archive, _ := m.st.UpsertFolder("A", "Archive", "archive", 3)
	ids := []int64{
		seedInbox(t, m, sent, "A", 1, "Me", "Sent from my phone", false),
		seedInbox(t, m, drafts, "A", 2, "Me", "Half-written", false),
		seedInbox(t, m, archive, "A", 3, "Alice", "Filed by a server rule", false),
	}
	startNotifier(t, m)
	signal(m, ids...)
	assertQuiet(t, got)
}
