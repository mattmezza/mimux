package mail

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/mattmezza/sm/internal/store"
)

// Notifications. Two transports hang off one trigger:
//
//   - Web Push — encrypted end-to-end to a specific browser install, delivered
//     by the vendor's push service. Needs HTTPS, a service worker and (on iOS)
//     a home-screen install.
//   - ntfy — one plain POST to a topic URL, which the ntfy app turns into a
//     notification. Needs none of the above, so it is the fallback for a device
//     that can't or won't do Web Push.
//
// Both are fed by notify() and gated by the same Prefs.NotifyScope master
// switch, which is "off" until the user says otherwise. Nothing here ever
// blocks a sync: every call site launches notify() in its own goroutine and the
// HTTP clients carry their own timeouts.

// notifyTimeout bounds a single push/ntfy POST. The sync loop has already moved
// on by then; this only stops goroutines piling up when a push service hangs.
const notifyTimeout = 10 * time.Second

// notifyTTL is how long a push service should hold an undelivered notification
// for a phone that is off or offline. Four hours: mail older than that is not
// worth buzzing about when the device finally comes back.
const notifyTTL = 4 * time.Hour

var notifyClient = &http.Client{Timeout: notifyTimeout}

// notify sends one notification through every configured transport. Blocking:
// callers run it in a goroutine. Errors are logged, never returned — a dead
// push service must not surface as a sync failure.
func (m *Manager) notify(account, from, subject string) {
	title := strings.TrimSpace(from)
	if title == "" {
		title = "New mail"
	}
	if account != "" {
		title += " · " + account
	}
	body := strings.TrimSpace(subject)
	if body == "" {
		body = "(no subject)"
	}
	body = truncateRunes(body, 140)
	if url := strings.TrimSpace(m.st.GetPrefs().NtfyURL); url != "" {
		m.notifyNtfy(url, title, body)
	}
	m.notifyPush(account, title, body)
}

// notifyNtfy posts to an ntfy topic. The message is the body; ntfy reads the
// headline off the Title header.
func (m *Manager) notifyNtfy(url, title, body string) {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		slog.Warn("notify: ntfy url must be http(s)", "url", url)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		slog.Warn("notify: ntfy request", "err", err)
		return
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "email")
	resp, err := notifyClient.Do(req)
	if err != nil {
		slog.Warn("notify: ntfy post", "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		slog.Warn("notify: ntfy rejected", "status", resp.StatusCode)
	}
}

// notifyPush encrypts and sends the payload to every registered device, and
// prunes the ones the push service says are gone. A 404/410 is the *only*
// documented signal that a subscription is dead: without acting on it the table
// fills with endpoints that are POSTed to forever.
func (m *Manager) notifyPush(account, title, body string) {
	subs, err := m.st.ListPushSubs()
	if err != nil || len(subs) == 0 {
		return
	}
	pub, priv := m.st.VAPIDKeys()
	if pub == "" || priv == "" {
		return // nothing has ever subscribed, so no keys were generated
	}
	// The payload is encrypted to each device's own key — the push service
	// relays ciphertext it cannot read. It still sees that this endpoint got a
	// message, when, and roughly how big: metadata, not content.
	payload, err := json.Marshal(map[string]string{"title": title, "body": body, "account": account})
	if err != nil {
		return
	}
	for _, s := range subs {
		sub := &webpush.Subscription{
			Endpoint: s.Endpoint,
			Keys:     webpush.Keys{P256dh: s.P256dh, Auth: s.Auth},
		}
		resp, err := webpush.SendNotification(payload, sub, &webpush.Options{
			HTTPClient:      notifyClient,
			Subscriber:      m.vapidSubscriber(),
			VAPIDPublicKey:  pub,
			VAPIDPrivateKey: priv,
			TTL:             int(notifyTTL / time.Second),
			Urgency:         webpush.UrgencyNormal,
			// Collapse queued notifications per account: a phone that was off
			// for an hour should light up once, not forty times.
			Topic: pushTopic(account),
		})
		if err != nil {
			slog.Warn("notify: push send", "err", err)
			continue
		}
		status := resp.StatusCode
		_ = resp.Body.Close()
		switch {
		case status == http.StatusNotFound || status == http.StatusGone:
			if err := m.st.DeletePushSub(s.Endpoint); err != nil {
				slog.Warn("notify: prune dead subscription", "err", err)
			} else {
				slog.Info("notify: pruned dead push subscription", "device", s.Label)
			}
		case status >= 300:
			slog.Warn("notify: push rejected", "status", status, "device", s.Label)
		}
	}
}

// pushTopic is the Topic header used to collapse pending notifications. It must
// be a short URL-safe token, so a non-conforming account name is dropped rather
// than sent (the header is optional; a rejected one would fail the whole POST).
func pushTopic(account string) string {
	for _, r := range account {
		urlSafe := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !urlSafe {
			return ""
		}
	}
	return truncateRunes(account, 32)
}

// vapidSubscriber is the "sub" claim in the VAPID JWT: who to contact about
// this push traffic. The public base URL when it's HTTPS (the only deployment
// where push works at all), else a placeholder for local development.
func (m *Manager) vapidSubscriber() string {
	if u := m.cfg.Server.BaseURL; strings.HasPrefix(u, "https://") {
		return u
	}
	return "mailto:sm@localhost"
}

// VAPIDPublicKey returns the instance's public VAPID key, generating the pair
// on first call. The browser needs it to subscribe; the private half never
// leaves the server (and never leaves the DB — see migration 0160).
func (m *Manager) VAPIDPublicKey() string {
	if pub, priv := m.st.VAPIDKeys(); pub != "" && priv != "" {
		return pub
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		slog.Error("notify: generate VAPID keys", "err", err)
		return ""
	}
	if err := m.st.SaveVAPIDKeys(pub, priv); err != nil {
		slog.Error("notify: persist VAPID keys", "err", err)
		return ""
	}
	slog.Info("notify: generated VAPID key pair")
	return pub
}

// NotifyTest sends a test notification through every configured transport, so
// the user can prove the plumbing works without waiting for mail. Explicitly
// user-initiated, so it ignores the NotifyScope master switch.
func (m *Manager) NotifyTest() {
	go m.notify("", "SM", "Test notification — notifications are working.")
}

// maybeNotify is the "tell me about every new message" path, called for each
// newly-stored message during a sync.
//
// The guards are the entire point of this function. Without them the first sync
// after a fresh install — or after a UIDVALIDITY reset makes the worker re-fetch
// a mailbox it already had — announces months of backfilled mail one buzz at a
// time:
//
//   - inbox only, or your own outgoing mail notifies you as it syncs back into
//     Sent, and Gmail's All Mail copy of every message notifies a second time;
//   - nothing until the account has completed one successful sync in this
//     process, which is what makes the initial backfill silent (cost: at most
//     one poll interval of silence after a restart);
//   - nothing already marked \Seen — it was read somewhere else;
//   - nothing older than a day, the backstop for a re-fetch of an old mailbox
//     that the LastSync guard doesn't cover (a UIDVALIDITY bump mid-session).
//
// The in-memory guards run before the scope is read, deliberately: this is
// called for every newly-stored message, GetPrefs is ~30 queries on the single
// shared connection, and the 500-message backfill it must stay out of is
// exactly the case notifiable() rejects without touching the database.
func (a *account) maybeNotify(f *store.Folder, buf *imapclient.FetchMessageBuffer) {
	if !notifiable(f, buf.Flags, buf.InternalDate, a.getStatus().LastSync) {
		return
	}
	if a.m.st.GetPrefs().NotifyScope != "all" {
		return
	}
	from := ""
	if buf.Envelope != nil && len(buf.Envelope.From) > 0 {
		from = addrDisplay(buf.Envelope.From[0])
	}
	subject := ""
	if buf.Envelope != nil {
		subject = buf.Envelope.Subject
	}
	go a.m.notify(a.cfg.Name, from, subject)
}

// notifiable holds the guards maybeNotify documents, split out because they are
// pure branching logic and exactly the kind of thing that silently regresses
// into notifying about backfill.
func notifiable(f *store.Folder, flags []imap.Flag, received, lastSync time.Time) bool {
	switch {
	case f == nil || f.SpecialUse != "inbox":
		return false
	case lastSync.IsZero():
		return false
	case hasFlag(flags, imap.FlagSeen):
		return false
	case received.IsZero() || time.Since(received) > 24*time.Hour:
		return false
	}
	return true
}

// addrDisplay is the notification headline for a sender: their name when the
// envelope carries one, else the bare address.
func addrDisplay(a imap.Address) string {
	if n := strings.TrimSpace(a.Name); n != "" {
		return n
	}
	return addrString(a)
}

// truncateRunes cuts s to at most n runes (never mid-rune), adding an ellipsis.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
