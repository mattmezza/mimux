// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

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
//
// RULES RUN ON MAIL ARRIVING IN AN INBOX, AND NOWHERE ELSE. Both gates are
// here, at the one place every caller goes through, rather than repeated per
// action (which is what the old notify-only inbox check was):
//
//   - arrival: fetchSet's announce flag, off for a folder's first full pass.
//     Without it, installing mimux — or ticking a folder in Settings, or a
//     mid-session UIDVALIDITY re-fetch — replays every rule over the whole
//     downloaded window: a "delete" rule empties an archive, a "forward" rule
//     mails a year of history to a colleague.
//   - inbox: the steady loop walks every synced folder now, so without this a
//     rule fires again on the Sent copy of your own reply, a third time on
//     Gmail's All Mail copy, and — worst — a move/delete rule eats the IMAP
//     draft you are still typing.
//
// ponytail: inbox-only also means server-side (sieve) delivery straight into a
// custom folder is not filtered here. Gate on a per-folder "rules run here"
// flag if someone actually needs it; a blanket "every folder" is what caused
// all three bugs above.
func (a *account) runRules(ctx context.Context, c *imapclient.Client, f *store.Folder, uid uint32, arrival bool) {
	if !arrival || f.SpecialUse != "inbox" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("filter: rule execution panicked", "account", a.cfg.Name, "err", r)
		}
	}()
	id, err := a.messageID(f.ID, uid)
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
		// moveTo owns the local row: it relocates it with the UID the server
		// assigned in the destination, or drops it when the server cannot say
		// (no UIDPLUS). Deleting it here first — which this used to do — left
		// the move announcing a message id that no longer resolved.
		return m.moveToFolder(ctx, c, msg, act.Arg)
	case filter.ActionDelete:
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
		// The rule says "tell me about this one"; the notifier owns everything
		// after that — the master switch, the one-window debounce, the dedup and
		// the wording (see notifyLoop). This used to be its own inline path with
		// its own guards and its own goroutine.
		m.hub.broadcast(Event{Type: "rule-notify", Data: strconv.FormatInt(msg.ID, 10)})
		return nil
	case filter.ActionLabel:
		// Same write path (and the same IMAP-push gap) as labelling by hand
		// from the reading pane — see SetLabel.
		return m.SetLabel(msg, act.Arg, true)
	default:
		return fmt.Errorf("filter: unknown action %q", act.Type)
	}
}

// forwardMessage sends msg on to a new recipient as part of a "forward"
// filter action, quoting the stored snippet. Unlike a forward the user asks
// for (Manager.QuoteSource, via server's prefillReply) this one runs inside the
// sync loop against every matching message, and a body fetch per match is not
// something the mailbox should wait on.
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
