package mail

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mattmezza/sm/internal/filter"
	"github.com/mattmezza/sm/internal/store"
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
func (a *account) runRules(ctx context.Context, folderID int64, uid uint32) {
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
		if err := a.m.applyAction(ctx, msg, act); err != nil {
			slog.Warn("filter: action failed", "account", a.cfg.Name, "action", act.Type, "err", err)
		}
	}
}

// applyAction runs a single filter action against msg. label is not
// implemented yet (needs Gmail label support, phase 4) — it's logged and
// skipped rather than treated as an error.
func (m *Manager) applyAction(ctx context.Context, msg *store.Message, act filter.Action) error {
	switch act.Type {
	case filter.ActionMarkRead:
		_ = m.st.SetRead(msg.ID, true)
		return m.SetRead(ctx, msg, true)
	case filter.ActionStar:
		_ = m.st.SetStarred(msg.ID, true)
		return m.SetStarred(ctx, msg, true)
	case filter.ActionMove:
		_ = m.st.DeleteMessage(msg.ID)
		return m.MoveToFolder(ctx, msg, act.Arg)
	case filter.ActionDelete:
		_ = m.st.DeleteMessage(msg.ID)
		return m.Move(ctx, msg, "trash")
	case filter.ActionForward:
		return m.forwardMessage(ctx, msg, act.Arg)
	case filter.ActionLabel:
		// NOTE: go-imap/v2 beta.8 can neither request nor STORE the Gmail
		// X-GM-LABELS extension item (its FETCH/STORE encoders reject unknown
		// atoms and the response parser errors on them), so applying a Gmail
		// label from a filter isn't wired. Logged+skipped for every provider;
		// implement once the library ships Gmail-extension support.
		slog.Info("filter: label action unsupported by the IMAP client; skipping", "arg", act.Arg)
		return nil
	default:
		return fmt.Errorf("filter: unknown action %q", act.Type)
	}
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
