// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/mimux/internal/filter"
	"github.com/mattmezza/mimux/internal/store"
)

// matchingActions returns, in rule position order, every action from every
// rule that matches meta. This is the pure decision the sync engine hands
// off to applyAction (which does the actual I/O) — kept separate so it's
// testable without a live account/store.
func matchingActions(rules []filter.Rule, meta filter.MessageMeta) []filter.Action {
	var out []filter.Action
	for _, r := range rules {
		if r.Matches(meta) {
			out = append(out, r.Actions...)
		}
	}
	return out
}

// runRules evaluates every enabled filter rule for the account against a
// newly-synced message and applies matching actions. It never lets sync
// break: a bad rule, an unknown action or a failed IMAP/SMTP call is logged
// and the loop continues with the next action/message.
//
// c is the sync's own connection: this runs on the worker goroutine, so the
// actions have to use it rather than the command queue only that goroutine
// drains (see account.exec).
func (a *account) runRules(ctx context.Context, c *imapclient.Client, folderID int64, uid uint32) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("filter: rule execution panicked", "account", a.cfg.Name, "err", r)
		}
	}()
	id, err := a.messageID(folderID, uid)
	if err != nil || id == 0 {
		return
	}
	msg, err := a.m.st.MessageByID(id)
	if err != nil || msg == nil {
		return
	}
	rules, err := a.m.st.RulesForAccount(a.cfg.Name)
	if err != nil || len(rules) == 0 {
		return
	}
	meta := filter.MessageMeta{From: msg.FromAddress, To: msg.ToAddresses, Subject: msg.Subject, Body: msg.Snippet}
	for _, act := range matchingActions(rules, meta) {
		if err := a.m.applyAction(ctx, c, msg, act); err != nil {
			slog.Warn("filter: action failed", "account", a.cfg.Name, "action", act.Type, "err", err)
		}
	}
}

// applyAction runs a single filter action against msg, on the connection the
// caller holds (see runRules and account.exec).
func (m *Manager) applyAction(ctx context.Context, c *imapclient.Client, msg *store.Message, act filter.Action) error {
	switch act.Type {
	case filter.ActionMarkRead:
		_ = m.st.SetRead(msg.ID, true)
		return m.setRead(ctx, c, msg, true)
	case filter.ActionStar:
		_, _ = m.st.SetStarred(msg.ID, true)
		return m.setStarred(ctx, c, msg, true)
	case filter.ActionMove:
		_ = m.st.DeleteMessage(msg.ID)
		return m.moveToFolder(ctx, c, msg, act.Arg)
	case filter.ActionDelete:
		_ = m.st.DeleteMessage(msg.ID)
		// "trash" resolves special-use-first, so this is Move(msg, "trash") with
		// the connection threaded through.
		return m.moveToFolder(ctx, c, msg, "trash")
	case filter.ActionForward:
		// Off the worker, like notifyForRule below: forwarding talks SMTP and then
		// APPENDs to Sent via a.submit, which the goroutine running this sync
		// cannot drain until it returns. NOTE: the copy + a logged error is
		// the whole error handling; give it the outbox treatment (store.Outbox +
		// the scheduler's retry) if forwards ever need to survive a restart.
		fwd := *msg
		go func() {
			if err := m.forwardMessage(ctx, &fwd, act.Arg); err != nil {
				slog.Warn("filter: forward failed", "account", fwd.Account, "to", act.Arg, "err", err)
			}
		}()
		return nil
	case filter.ActionNotify:
		return m.notifyForRule(msg)
	case filter.ActionLabel:
		// Same write path (and the same IMAP-push gap) as labelling by hand
		// from the reading pane — see SetLabel.
		return m.SetLabel(msg, act.Arg, true)
	default:
		return fmt.Errorf("filter: unknown action %q", act.Type)
	}
}

// notifyForRule is the "notify" filter action: a rule matched, so tell the user
// about this message. Two gates, both there to stop the same message buzzing
// twice:
//
//   - only under the "rules" scope. Under "all" every new inbox message already
//     notifies (see Manager.flushNotify), which makes the action redundant, not
//     additive; under "off" nothing notifies at all.
//   - inbox only. Rules run for every folder a sync touches, so without this a
//     rule matching your correspondent fires again on the Sent copy of your own
//     reply, and a third time on Gmail's All Mail copy.
func (m *Manager) notifyForRule(msg *store.Message) error {
	if m.st.GetPrefs().NotifyScope != "rules" {
		return nil
	}
	f, err := m.st.FolderByID(msg.FolderID)
	if err != nil || f == nil || f.SpecialUse != "inbox" {
		return nil
	}
	from := msg.FromName
	if from == "" {
		from = msg.FromAddress
	}
	// Fire and forget: applyAction runs inside the sync loop, and a push service
	// that takes ten seconds to answer must not hold up the mailbox.
	go m.notify(msg.Account, from, msg.Subject, messageLink(m.cfg.Server.BaseURL, msg.FolderID, msg.ID))
	return nil
}

// forwardMessage sends msg on to a new recipient as part of a "forward"
// filter action, quoting the stored snippet (the full body isn't fetched
// for this — see note on prefillReply in server/compose.go).
func (m *Manager) forwardMessage(ctx context.Context, msg *store.Message, to string) error {
	from := msg.FromAddress
	if msg.FromName != "" {
		from = msg.FromName + " <" + msg.FromAddress + ">"
	}
	in := ComposeInput{
		To:      []string{to},
		Subject: PrefixSubject("forward", msg.Subject),
		Body: "This message was automatically forwarded by a filter rule.\n\n" +
			QuoteBody(msg.Date, from, msg.Snippet),
	}
	_, err := m.Send(ctx, msg.Account, in)
	return err
}
