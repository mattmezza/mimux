package mail

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/store"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewManager(&config.Config{}, st)
}

// fakeSubscription mints a real P-256 keypair and auth secret, so the
// encryption path in webpush-go runs for real rather than being stubbed —
// a subscription with junk keys fails before the HTTP request is ever made,
// which would make the pruning test pass for the wrong reason.
func fakeSubscription(t *testing.T, endpoint string) store.PushSub {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	return store.PushSub{
		Endpoint: endpoint,
		P256dh:   base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
		Label:    "Test device",
	}
}

// TestNotifyPushPrunesGoneSubscription is the one that matters: a push service
// answering 410 means the subscription is dead forever, and a row that isn't
// deleted is one SM keeps POSTing to on every single new message.
func TestNotifyPushPrunesGoneSubscription(t *testing.T) {
	m := testManager(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// A real push service gets an encrypted body and the VAPID auth header.
		if body, _ := io.ReadAll(r.Body); len(body) == 0 {
			t.Error("push request had an empty (unencrypted?) body")
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("push request carried no VAPID Authorization header")
		}
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	if m.VAPIDPublicKey() == "" {
		t.Fatal("no VAPID key generated")
	}
	if err := m.st.SavePushSub(fakeSubscription(t, srv.URL)); err != nil {
		t.Fatal(err)
	}
	m.notifyPush("Gmail", "Someone", "Hello", "")

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("push service hit %d times, want 1", got)
	}
	subs, err := m.st.ListPushSubs()
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 0 {
		t.Fatalf("subscription survived a 410: %+v", subs)
	}
}

// A transient failure is not a dead subscription: a 500 must leave the row
// alone or one bad afternoon at the push service unsubscribes every device.
func TestNotifyPushKeepsSubscriptionOnServerError(t *testing.T) {
	m := testManager(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	m.VAPIDPublicKey()
	if err := m.st.SavePushSub(fakeSubscription(t, srv.URL)); err != nil {
		t.Fatal(err)
	}
	m.notifyPush("Gmail", "Someone", "Hello", "")

	subs, _ := m.st.ListPushSubs()
	if len(subs) != 1 {
		t.Fatalf("subscription pruned on a 500: %d left", len(subs))
	}
}

// TestNotifyNtfy is the whole ntfy transport end to end: a local listener
// standing in for the ntfy server, asserting the title and body actually
// arrive.
func TestNotifyNtfy(t *testing.T) {
	m := testManager(t)
	type got struct{ title, body, click string }
	ch := make(chan got, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- got{r.Header.Get("Title"), string(b), r.Header.Get("Click")}
	}))
	defer srv.Close()

	prefs := m.st.GetPrefs()
	prefs.NtfyURL = srv.URL
	if err := m.st.SavePrefs(prefs); err != nil {
		t.Fatal(err)
	}
	m.notify("Gmail", "Alice", "Lunch?", "https://mail.example.com/?t=42&src=7")

	select {
	case g := <-ch:
		if g.title != "Alice · Gmail" {
			t.Errorf("Title = %q", g.title)
		}
		if g.body != "Lunch?" {
			t.Errorf("body = %q", g.body)
		}
		// Without Click the notification opens nothing on tap.
		if g.click != "https://mail.example.com/?t=42&src=7" {
			t.Errorf("Click = %q", g.click)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ntfy was never called")
	}
}

// A non-http(s) topic URL must be refused rather than posted to.
func TestNotifyNtfyRejectsNonHTTP(t *testing.T) {
	m := testManager(t)
	m.notifyNtfy("file:///etc/passwd", "t", "b", "") // must not panic or dial
}

// TestNotifiableGuards pins the guards that keep a first sync — or a
// UIDVALIDITY re-fetch — from announcing months of backfilled mail.
func TestNotifiableGuards(t *testing.T) {
	inbox := &store.Folder{SpecialUse: "inbox"}
	sent := &store.Folder{SpecialUse: "sent"}
	lastSync := time.Now().Add(-time.Minute)
	fresh := time.Now().Add(-time.Minute)

	cases := []struct {
		name     string
		f        *store.Folder
		flags    []imap.Flag
		received time.Time
		lastSync time.Time
		want     bool
	}{
		{"new inbox mail", inbox, nil, fresh, lastSync, true},
		{"sent copy of my own mail", sent, nil, fresh, lastSync, false},
		{"no folder", nil, nil, fresh, lastSync, false},
		{"first sync backfill", inbox, nil, fresh, time.Time{}, false},
		{"already read elsewhere", inbox, []imap.Flag{imap.FlagSeen}, fresh, lastSync, false},
		{"old mail re-fetched", inbox, nil, time.Now().Add(-72 * time.Hour), lastSync, false},
		{"no internal date", inbox, nil, time.Time{}, lastSync, false},
		{"flagged but unread", inbox, []imap.Flag{imap.FlagFlagged}, fresh, lastSync, true},
	}
	for _, c := range cases {
		if got := notifiable(c.f, c.flags, c.received, c.lastSync); got != c.want {
			t.Errorf("%s: notifiable = %v, want %v", c.name, got, c.want)
		}
	}
}

// The scope switch is the master off: with it at its default nothing is sent,
// which is what "no notifications unless you ask" actually rests on.
func TestNotifyScopeDefaultsOff(t *testing.T) {
	m := testManager(t)
	if got := m.st.GetPrefs().NotifyScope; got != "off" {
		t.Fatalf("default NotifyScope = %q, want %q", got, "off")
	}
}

// The tap target: it must be absolute (ntfy's Click header is opened by a phone
// that has no notion of this app's origin), must survive a base URL with a
// trailing slash, and must be the ?t=<id>&src=<folder> deep link app.js knows
// how to restore — a wrong shape silently opens the plain inbox.
func TestMessageLink(t *testing.T) {
	if got := messageLink("https://mail.example.com", 7, 42); got != "https://mail.example.com/?t=42&src=7" {
		t.Errorf("messageLink = %q", got)
	}
	if got := messageLink("https://mail.example.com/", 7, 42); got != "https://mail.example.com/?t=42&src=7" {
		t.Errorf("messageLink with trailing slash = %q", got)
	}
	// Nothing to open: better no link than a link to nowhere.
	if got := messageLink("", 7, 42); got != "" {
		t.Errorf("messageLink without a base URL = %q", got)
	}
	if got := messageLink("https://mail.example.com", 7, 0); got != "" {
		t.Errorf("messageLink without a message id = %q", got)
	}
}

func TestPushTopic(t *testing.T) {
	if got := pushTopic("Gmail"); got != "Gmail" {
		t.Errorf("pushTopic(Gmail) = %q", got)
	}
	// A header value that isn't URL-safe would make the push service reject the
	// whole POST, so it is dropped instead of sent.
	if got := pushTopic("Matteo Merola"); got != "" {
		t.Errorf("pushTopic with a space = %q, want empty", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Errorf("truncateRunes = %q", got)
	}
	if got := truncateRunes("héllo wörld", 5); got != "héllo…" {
		t.Errorf("truncateRunes = %q", got)
	}
}
