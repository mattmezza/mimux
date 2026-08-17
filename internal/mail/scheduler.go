package mail

import (
	"context"
	"log/slog"
	"time"

	"github.com/mattmezza/mimux/internal/store"
)

// schedulerTick is how often the outbox is drained. 5s is plenty: undo windows
// are 3–10s and we compare send_at <= now, so a message goes out within one
// tick of becoming due.
const schedulerTick = 5 * time.Second

// runScheduler drains due outbox rows until ctx is cancelled. Survives restarts
// because it re-reads queued rows from the DB on every tick.
func (m *Manager) runScheduler(ctx context.Context) {
	// Before the first drain: anything still "sending" was claimed by a
	// previous run that died mid-send, so it is stranded. See RecoverSending.
	if n, err := m.st.RecoverSending(); err != nil {
		slog.Error("scheduler: recover stranded", "err", err)
	} else if n > 0 {
		slog.Warn("scheduler: rows stranded mid-send moved to failed", "n", n)
	}
	m.dispatchDue(ctx) // catch anything already overdue at startup
	t := time.NewTicker(schedulerTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.dispatchDue(ctx)
		}
	}
}

// dispatchDue claims and sends every message whose time has come. A claim
// (queued -> sending) guards against a slow send overlapping the next tick and
// double-delivering. Failures mark the row and surface a toast so a broken
// account can't silently eat scheduled mail.
func (m *Manager) dispatchDue(ctx context.Context) {
	due, err := m.st.DueOutbox(time.Now().UTC())
	if err != nil {
		slog.Error("scheduler: due", "err", err)
		return
	}
	for i := range due {
		o := due[i]
		claimed, err := m.st.ClaimOutbox(o.ID)
		if err != nil || !claimed {
			continue
		}
		in := composeInputFromOutbox(o)
		if _, err := m.Send(ctx, o.Account, in); err != nil {
			slog.Error("scheduler: send", "id", o.ID, "account", o.Account, "err", err)
			_ = m.st.MarkOutboxFailed(o.ID, err.Error())
			m.Toast("Scheduled message \"" + subjectOrNoSubject(o.Subject) + "\" failed to send: " + err.Error())
			continue
		}
		_ = m.st.MarkOutboxSent(o.ID)
		m.hub.broadcast(Event{Type: "new-mail", Data: o.Account}) // refresh Sent view
	}
}

func subjectOrNoSubject(s string) string {
	if s == "" {
		return "(no subject)"
	}
	return s
}

// composeInputFromOutbox rebuilds the send payload from a stored outbox row.
func composeInputFromOutbox(o store.Outbox) ComposeInput {
	atts := make([]OutAttachment, len(o.Attachments))
	for i, a := range o.Attachments {
		atts[i] = OutAttachment{Filename: a.Filename, ContentType: a.ContentType, Data: a.Data}
	}
	return ComposeInput{
		To: SplitAddrList(o.To), Cc: SplitAddrList(o.Cc), Bcc: SplitAddrList(o.Bcc),
		Subject: o.Subject, Body: o.Body, Mode: o.Mode, From: o.From,
		InReplyTo: o.InReplyTo, References: o.References, Attachments: atts,
	}
}
