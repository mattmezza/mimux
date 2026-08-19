// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/mattmezza/mimux/internal/store"
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

// notify sends one notification through every configured transport. link is the
// absolute URL a tap should open (empty for "just open the app"). Blocking:
// callers run it in a goroutine. Errors are logged, never returned — a dead
// push service must not surface as a sync failure.
func (m *Manager) notify(account, from, subject, link string) {
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
		m.notifyNtfy(url, title, body, link)
	}
	m.notifyPush(account, title, body, link)
}

// notifyNtfy posts to an ntfy topic. The message is the body; ntfy reads the
// headline off the Title header and the tap target off Click.
func (m *Manager) notifyNtfy(url, title, body, link string) {
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
	if link != "" {
		req.Header.Set("Click", link)
	}
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
func (m *Manager) notifyPush(account, title, body, link string) {
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
	payload, err := json.Marshal(map[string]string{"title": title, "body": body, "account": account, "url": link})
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

// messageLink is the absolute URL that opens one message: the inbox page with
// the message's conversation deep-linked into the reading pane (?t=<id>&src=
// <folder>, the same shape the list pushes into history — see app.js). Absolute
// because ntfy's Click header is opened by a phone that knows nothing about
// this app's origin; the service worker re-bases it onto its own origin.
func messageLink(baseURL string, folderID, msgID int64) string {
	if baseURL == "" || msgID == 0 {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/?t=" + strconv.FormatInt(msgID, 10) +
		"&src=" + strconv.FormatInt(folderID, 10)
}

// vapidSubscriber is the "sub" claim in the VAPID JWT: who to contact about
// this push traffic. The public base URL when it's HTTPS (the only deployment
// where push works at all), else a placeholder for local development.
func (m *Manager) vapidSubscriber() string {
	if u := m.cfg.Server.BaseURL; strings.HasPrefix(u, "https://") {
		return u
	}
	return "mailto:mimux@localhost"
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
	// NOTE: no link — a test notification has no message to open, so a tap
	// falls back to "just open the app".
	go m.notify("", "mimux", "Test notification — notifications are working.", "")
}

// signalNewMessage announces a just-stored message on the hub, by store id, so
// a subscriber can react to *which* message arrived instead of only "something
// changed" (which is all the new-mail event has ever said). The notifier
// batches the ids into one buzz; pro/webhooks.go turns each into a payload.
//
// Not folder-filtered on purpose — a subscriber knows what it cares about, and
// the id resolves to the folder anyway — but it is backfill-filtered, or a
// fresh install would announce months of old mail one event at a time. Two
// guards do that: nothing until the account has completed a successful sync in
// this process (which is what makes the initial backfill silent, at a cost of
// at most one poll interval of silence after a restart), and nothing older than
// a day (the backstop for a mid-session UIDVALIDITY re-fetch).
//
// Everything else is the subscriber's policy, not this function's: the notifier
// wants unread inbox mail only, a webhook subscriber wants to hear about the
// Sent copy too. See flushNotify.
func (a *account) signalNewMessage(f *store.Folder, buf *imapclient.FetchMessageBuffer) {
	if a.getStatus().LastSync.IsZero() ||
		buf.InternalDate.IsZero() || time.Since(buf.InternalDate) > 24*time.Hour {
		return
	}
	msg, err := a.m.st.MessageByFolderUID(f.ID, uint32(buf.UID))
	if err != nil || msg == nil {
		return
	}
	a.m.hub.broadcast(Event{Type: "message-new", Data: strconv.FormatInt(msg.ID, 10)})
}

// truncateRunes cuts s to at most n runes (never mid-rune), adding an ellipsis.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
