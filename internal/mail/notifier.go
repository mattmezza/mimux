// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mattmezza/mimux/internal/store"
)

// The notification trigger. One goroutine, started from Manager.Start, reading
// message-new off the hub — the same event the webhook engine consumes — and
// buzzing at most once per window.
//
// It used to fire inline from the sync loop, once per newly-stored message:
// forty messages landing in one cycle was forty notifications, each one
// preceded by its own GetPrefs() (~47 queries on the single shared SQLite
// connection) in the middle of the fetch. Batching is the whole point, and the
// bus already carries exactly the event needed for it.
//
// The transports below (notify/notifyNtfy/notifyPush) are unchanged: only what
// decides to call them moved.

// notifyWindow is the trailing debounce: the flush happens a minute after the
// last message arrives, so a sync cycle that stores twenty messages produces
// one notification rather than twenty.
const notifyWindow = time.Minute

// notifyDetail is how many senders a batch names before it says "and N more".
// Three fits in the two lines a phone shows without expanding.
const notifyDetail = 3

// runNotifier subscribes and runs the loop. Lossy on purpose: a missed buzz is
// a missed buzz, and a notification nobody got is not worth stalling the sync
// engine's broadcast for.
func (m *Manager) runNotifier(ctx context.Context) {
	events, unsubscribe := m.hub.subscribe()
	defer unsubscribe()
	m.notifyLoop(ctx, events, notifyWindow)
}

// notifyLoop accumulates message ids and flushes them one window after the last
// one. Split from runNotifier — and taking the window rather than reading it —
// so a test can subscribe first and then broadcast, instead of racing the
// subscription, and need not wait a minute for the flush.
func (m *Manager) notifyLoop(ctx context.Context, events <-chan Event, window time.Duration) {
	pending := map[int64]struct{}{}
	t := time.NewTimer(window)
	t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Type != "message-new" {
				continue
			}
			id, err := strconv.ParseInt(ev.Data, 10, 64)
			if err != nil {
				continue
			}
			pending[id] = struct{}{} // dedup: the same message can be signalled twice
			t.Reset(window)
		case <-t.C:
			m.flushNotify(pending)
			clear(pending)
		}
	}
}

// flushNotify turns one window's worth of message ids into at most one
// notification.
//
// The freshness guards (the account has synced once, the message is less than a
// day old) already ran in signalNewMessage, which is what keeps a first sync or
// a UIDVALIDITY re-fetch silent. What is left is notification policy, and it
// belongs here: inbox only — or your own outgoing mail buzzes as it syncs back
// into Sent, and Gmail's All Mail copy buzzes a second time. That filter is
// load-bearing now that the steady loop re-reads the whole selected folder set
// every cycle rather than only the inbox: the Sent copy turns up within the
// minute, not on the next reconnect. See TestOnlyInboxMailBuzzes. And nothing
// already read, which means read somewhere else, or read here in the minute
// this window was open.
func (m *Manager) flushNotify(ids map[int64]struct{}) {
	if len(ids) == 0 {
		return
	}
	// Once per window, not once per message: this is the expensive read the old
	// per-message path made forty times a cycle.
	if m.st.GetPrefs().NotifyScope != "all" {
		return
	}
	msgs := make([]*store.Message, 0, len(ids))
	for id := range ids {
		msg, err := m.st.MessageByID(id)
		if err != nil || msg == nil || msg.IsRead {
			continue
		}
		f, err := m.st.FolderByID(msg.FolderID)
		if err != nil || f == nil || f.SpecialUse != "inbox" {
			continue
		}
		msgs = append(msgs, msg)
	}
	if len(msgs) == 0 {
		return
	}
	// Newest first — map order is random and the batch body names the first few.
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].Date.After(msgs[j].Date) })
	if len(msgs) == 1 {
		msg := msgs[0]
		go m.notify(m.accountLabel(msg.Account), fromDisplay(msg), msg.Subject,
			messageLink(m.cfg.Server.BaseURL, msg.FolderID, msg.ID))
		return
	}
	// A batch has no one message to open, so it opens the All inboxes screen —
	// and no one account, so the title carries the count instead of a label.
	go m.notify("", fmt.Sprintf("%d new messages", len(msgs)), batchBody(msgs),
		allInboxesLink(m.cfg.Server.BaseURL))
}

// batchBody names the first few senders and what they wrote, so the
// notification is worth reading without opening the app. Both transports take a
// multi-line body: ntfy renders it as the message, Web Push as the payload body.
func batchBody(msgs []*store.Message) string {
	var b strings.Builder
	for i, msg := range msgs {
		if i == notifyDetail {
			fmt.Fprintf(&b, "…and %d more", len(msgs)-notifyDetail)
			break
		}
		if i > 0 {
			b.WriteString("\n")
		}
		subject := strings.TrimSpace(msg.Subject)
		if subject == "" {
			subject = "(no subject)"
		}
		fmt.Fprintf(&b, "%s — %s", fromDisplay(msg), subject)
	}
	return b.String()
}

// fromDisplay is the sender as a person: their name when the message carries
// one, else the bare address.
func fromDisplay(msg *store.Message) string {
	if n := strings.TrimSpace(msg.FromName); n != "" {
		return n
	}
	return msg.FromAddress
}

// accountLabel is the account's display label, falling back to its name when no
// worker is running for it (nothing is syncing, so this is a test or a
// just-removed account).
func (m *Manager) accountLabel(name string) string {
	if a := m.account(name); a != nil {
		return a.cfg.DisplayLabel()
	}
	return name
}

// allInboxesLink is the absolute URL of the All inboxes screen — the app's root
// page, which is where the sidebar's unified entry pushes to. Absolute for the
// same reason messageLink is.
func allInboxesLink(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/"
}
