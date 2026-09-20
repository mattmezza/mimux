// SPDX-License-Identifier: AGPL-3.0-only
// Package mail implements the IMAP sync engine, body fetching and HTML
// sanitization for mimux.
package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

// Event is pushed to SSE subscribers.
type Event struct {
	Type string // "sync-status" | "new-mail" (any list change) | "message-new" | "message-updated" | "toast" | search-*
	Data string // account name; the store message id for "message-new", "<id> <change>" for "message-updated", text for "toast"
}

// AccountStatus is the live sync state surfaced in the status bar.
type AccountStatus struct {
	Account  string
	State    string // "ok" | "syncing" | "error"
	Message  string
	LastSync time.Time // last successful ("ok") sync; zero until first success
}

// Manager owns one worker per account (accounts live in the DB now) and a shared
// body cache. Workers are started/stopped at runtime via Reload when the account
// list changes in the Settings GUI.
type Manager struct {
	cfg    *config.Config // bootstrap only (Server.BaseURL for OAuth redirects)
	st     *store.Store
	bodies *bodyLRU
	hub    *hub

	// NOTE: one mutex guards the map + ctx; account changes are rare and
	// single-user, so this never contends. Off-limits smtp.go/compose.go read
	// m.accounts unlocked — acceptable given account edits don't overlap sends.
	mu       sync.Mutex
	ctx      context.Context //nolint:containedctx // root ctx retained so Reload can start new workers
	accounts map[string]*account
}

func NewManager(cfg *config.Config, st *store.Store) *Manager {
	return &Manager{
		cfg:      cfg,
		st:       st,
		bodies:   newBodyLRU(32),
		hub:      newHub(),
		accounts: map[string]*account{},
	}
}

// Start records the root context and launches a worker per DB account.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.Reload()
	go m.runScheduler(ctx)
	go m.runNotifier(ctx)
}

// Reload reconciles the running workers with the accounts in the DB: it starts
// workers for new accounts, stops those removed, and restarts any whose config
// changed. Called at startup and after every account edit in the GUI.
func (m *Manager) Reload() {
	accts, err := m.st.ListAccounts()
	if err != nil {
		return
	}
	want := make(map[string]config.Account, len(accts))
	for _, a := range accts {
		want[a.Name] = a
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil { // Reload before Start: nothing to launch yet
		return
	}
	for name, a := range m.accounts {
		w, ok := want[name]
		if !ok || !accountEqual(a.cfg, w) {
			a.cancel()
			delete(m.accounts, name)
		}
	}
	for name, ac := range want {
		if _, ok := m.accounts[name]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(m.ctx)
		a := &account{
			cfg:    ac,
			m:      m,
			cmds:   make(chan cmd, 64),
			wake:   make(chan struct{}, 1),
			nudge:  make(chan struct{}, 1),
			warm:   make(chan struct{}, 1),
			cancel: cancel,
			status: AccountStatus{Account: ac.Name, State: "syncing"},
		}
		m.accounts[name] = a
		go a.run(ctx)
		go a.runWarmer(ctx)
	}
}

// accountEqual reports whether two account configs are identical, including
// aliases (slices, so not comparable with ==).
func accountEqual(a, b config.Account) bool {
	if a.Name != b.Name || a.SenderName != b.SenderName || a.Provider != b.Provider ||
		a.Email != b.Email || a.Auth != b.Auth || a.Password != b.Password ||
		a.OAuth2ClientID != b.OAuth2ClientID || a.OAuth2ClientSecret != b.OAuth2ClientSecret ||
		a.IMAPHost != b.IMAPHost || a.IMAPPort != b.IMAPPort ||
		a.SMTPHost != b.SMTPHost || a.SMTPPort != b.SMTPPort ||
		len(a.Aliases) != len(b.Aliases) ||
		!intPtrEqual(a.SyncIntervalMin, b.SyncIntervalMin) ||
		!intPtrEqual(a.MaxPerSync, b.MaxPerSync) ||
		!intPtrEqual(a.SyncMonths, b.SyncMonths) ||
		!intPtrEqual(a.BodyCache, b.BodyCache) {
		return false
	}
	for i := range a.Aliases {
		if a.Aliases[i] != b.Aliases[i] {
			return false
		}
	}
	return true
}

// intPtrEqual compares two optional overrides: nil counts as different from
// any set value, even 0.
func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// account looks up a running worker under the lock.
func (m *Manager) account(name string) *account {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accounts[name]
}

// Status returns a snapshot of every account's sync state, sorted by name.
func (m *Manager) Status() []AccountStatus {
	m.mu.Lock()
	workers := make([]*account, 0, len(m.accounts))
	for _, a := range m.accounts {
		workers = append(workers, a)
	}
	m.mu.Unlock()
	out := make([]AccountStatus, 0, len(workers))
	for _, a := range workers {
		out = append(out, a.getStatus())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// AnySyncing reports whether at least one account is mid-sync — the single
// truth the "Syncing…" spinner renders. Lives here, next to the statuses it
// reads, so the SSE relay and the page render can't compute it differently.
func (m *Manager) AnySyncing() bool {
	for _, st := range m.Status() {
		if st.State == "syncing" {
			return true
		}
	}
	return false
}

// Subscribe returns an event channel and an unsubscribe func for SSE. Lossy:
// a subscriber that falls behind loses its oldest events.
func (m *Manager) Subscribe() (<-chan Event, func()) { return m.hub.subscribe() }

// SubscribeDurable is Subscribe for a consumer that acts on each event once and
// has no way to notice a missing one — the webhook engine. The broadcaster
// waits for it instead of dropping.
func (m *Manager) SubscribeDurable() (<-chan Event, func()) { return m.hub.subscribeDurable() }

// Wake nudges an account worker to retry — used after an OAuth authorization
// completes so a worker parked in the "authorize needed" state reconnects.
func (m *Manager) Wake(accountName string) {
	if a := m.account(accountName); a != nil {
		a.signalWake()
		a.signalWarm() // the warmer parks on ErrNoToken too
	}
}

// RefreshAll nudges every account worker to sync now — the only client-driven
// sync trigger there is ("Refresh now", pull-to-refresh). Background freshness
// is the workers' own job (IDLE + poll interval), so nothing else asks. It
// flips each account to "syncing" immediately so the status bar and health
// panel reflect the refresh right away; the worker sets "ok" when done.
//
// An account already in "error" is left alone: its worker is in connect-backoff
// (or parked on "authorize needed"), so painting it "syncing" would throw away
// the one diagnostic the health panel has and show motion that isn't happening.
// The wake still goes out and sleepOrWake cuts the backoff short, so an explicit
// refresh does retry a broken account now — it just keeps saying "error" until
// the retry has something new to report.
func (m *Manager) RefreshAll() {
	m.mu.Lock()
	workers := make([]*account, 0, len(m.accounts))
	for _, a := range m.accounts {
		workers = append(workers, a)
	}
	m.mu.Unlock()
	for _, a := range workers {
		if a.getStatus().State != "error" {
			a.setStatus("syncing", "")
		}
		a.signalWake()
	}
}

// --- account worker ---

type cmd struct {
	fn   func(*imapclient.Client) error
	done chan error
}

type account struct {
	cfg  config.Account
	m    *Manager
	cmds chan cmd
	// wake means "sync now": new data announced during IDLE, an explicit
	// refresh, or a queued command that changes mailbox state. nudge only means
	// "a read-only command is queued" — it breaks IDLE so the command runs, and
	// nothing else. Two channels, because the loop has to tell the two apart and
	// the wake token must survive being woken for a read-only command.
	wake   chan struct{}
	nudge  chan struct{}
	warm   chan struct{}      // nudges the background body warmer (see warm.go)
	cancel context.CancelFunc // stops this worker + its warmer (Reload/remove)

	mu     sync.Mutex
	status AccountStatus

	// sweep is the sync loop's own bookkeeping: where a budgeted sweep stopped
	// (see sweepFolders), which folders have had a deep pass, and the server's
	// message count per folder as of the last cycle that reconciled it — the
	// free expunge signal syncFolder reads out of SELECT. Worker-goroutine state:
	// session/steady own it, and the only other drivers of syncFolder (the
	// commands submit drains, tests) run one at a time.
	sweep sweepState
}

// sweepState is the state a sweep carries between cycles. Its maps are created
// lazily, so a fresh account (and every test) starts from "nothing known yet".
type sweepState struct {
	// resume is the folder a truncated sweep stopped before; "" means the next
	// sweep starts at the top of the rotation.
	resume string
	// reconnect is a pending pass over every discovered folder, including those
	// excluded from the regular synced set.
	reconnect bool
	// deep is when each folder last had a deep pass — the whole-mailbox
	// reconciliation that heals gaps and re-baselines the expunge signal.
	deep map[int64]time.Time
	// counts is the server's message count for each folder as of the last cycle
	// that reconciled it. The next cycle compares it with SELECT's count to tell
	// "nothing left the mailbox" (cheap, every cycle) from "something did"
	// (worth the whole-mailbox UID diff).
	counts map[int64]uint32
}

func (a *account) getStatus() AccountStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

func (a *account) setStatus(state, msg string) {
	a.mu.Lock()
	// setStatus rebuilds the struct each call; carry LastSync forward and stamp
	// it on a successful sync. NOTE: kept in one place, under the lock.
	last := a.status.LastSync
	if state == "ok" {
		last = time.Now()
	}
	a.status = AccountStatus{Account: a.cfg.Name, State: state, Message: msg, LastSync: last}
	a.mu.Unlock()
	a.m.hub.broadcast(Event{Type: "sync-status", Data: a.cfg.Name})
}

// signalListChanged tells subscribed browsers to re-fetch their open message
// list (new mail, flag changes or expunges). Reuses the "new-mail" event the
// client already maps to mimux:refresh + an unread-title refresh. Callers coalesce
// to at most one per sync cycle to avoid a refresh storm.
func (a *account) signalListChanged() {
	a.m.hub.broadcast(Event{Type: "new-mail", Data: a.cfg.Name})
	// Same trigger the warmer wants: the message list gained/changed rows, so
	// there may be inbox bodies to cache. A spurious nudge costs one query.
	a.signalWarm()
}

func (a *account) signalWake() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *account) signalNudge() {
	select {
	case a.nudge <- struct{}{}:
	default:
	}
}

// run is the connect/backoff supervisor. It never returns until ctx is done and
// never propagates a provider error to a crash — errors become status updates.
func (a *account) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		c, err := a.connect()
		if err != nil {
			if errors.Is(err, ErrNoToken) {
				// Not a failure to retry: wait for the OAuth callback to wake us.
				a.setStatus("error", "authorize needed")
				select {
				case <-ctx.Done():
					return
				case <-a.wake:
				}
				continue
			}
			a.setStatus("error", "connect: "+err.Error())
			if a.sleepOrWake(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = time.Second
		err = a.session(ctx, c)
		_ = c.Logout()
		_ = c.Close()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			a.setStatus("error", err.Error())
			slog.Warn("imap session ended", "account", a.cfg.Name, "err", err)
		} else {
			a.setStatus("error", "disconnected")
		}
		if a.sleepOrWake(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func (a *account) connect() (*imapclient.Client, error) {
	opts := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			// New mail (or flag changes) during IDLE: wake the loop to resync.
			Mailbox: func(*imapclient.UnilateralDataMailbox) { a.signalWake() },
		},
	}
	addr := fmt.Sprintf("%s:%d", a.cfg.IMAPHost, a.cfg.IMAPPort)
	c, err := imapclient.DialTLS(addr, opts)
	if err != nil {
		return nil, err
	}
	if err := a.authenticate(c); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// authenticate is the pluggable auth seam. Password today; OAuth2/XOAUTH2 slots
// in here later by switching on a.cfg.Auth.
func (a *account) authenticate(c *imapclient.Client) error {
	switch a.cfg.Auth {
	case "", "password":
		return c.Login(a.cfg.Email, a.cfg.Password).Wait()
	case "oauth2":
		tok, err := a.m.accessToken(context.Background(), a.cfg.Name)
		if err != nil {
			return err
		}
		return c.Authenticate(newXOAUTH2Client(a.cfg.Email, tok))
	default:
		return fmt.Errorf("unsupported auth mechanism %q", a.cfg.Auth)
	}
}

// session starts a bounded pass over all discovered folders, then lets steady
// finish any remaining reconnect work over subsequent cycles.
func (a *account) session(ctx context.Context, c *imapclient.Client) error {
	return a.sessionWithBudget(ctx, c, sweepBudget)
}

func (a *account) sessionWithBudget(ctx context.Context, c *imapclient.Client, budget time.Duration) error {
	a.setStatus("syncing", "")
	folders, err := a.syncFolders(c)
	if err != nil {
		return err
	}
	caps := c.Caps()
	// Preserve an unfinished pass across a connection drop. A fresh process
	// starts at the inbox and completes the rest in budgeted cycles.
	if !a.sweep.reconnect {
		a.sweep.resume = "" // a steady-state resume belongs to a different folder set
	}
	a.sweep.reconnect = true
	changed, resume, err := a.sweepFolderSet(ctx, c, caps, folders, a.sweep.resume, budget)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	a.sweep.resume = resume
	a.sweep.reconnect = resume != ""
	if changed {
		a.signalListChanged()
	}
	a.setStatus("ok", "")
	return a.steadyWithBudget(ctx, c, caps, budget)
}

// syncedFolders is the set the steady loop walks, INBOX LAST. The steady loop
// hoists the inbox back to the front itself (see sweepFolders) — this order is
// what the budgeted rotation over the remaining folders follows, and it keeps a
// folder ticked in Settings → Syncing behind the ones already being visited.
func (a *account) syncedFolders() ([]store.Folder, error) {
	folders, err := a.m.st.SyncedFolders(a.cfg.Name)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(folders, func(i, j int) bool {
		return folders[i].SpecialUse != "inbox" && folders[j].SpecialUse == "inbox"
	})
	return folders, nil
}

// selectInbox re-SELECTs the inbox when the selection has drifted, so IDLE
// always waits on the mailbox new mail arrives in. It drifts both ways: the
// cycle's own round-robin, and the read-only commands (a server-side search, an
// attachment listing) that SELECT another mailbox on this same connection and
// leave it selected. Cheap and idempotent — one local row read and a name
// compare when nothing moved.
func (a *account) selectInbox(c *imapclient.Client) error {
	f, err := a.m.st.FolderBySpecial(a.cfg.Name, "inbox")
	if err != nil || f == nil {
		return err
	}
	if mbox := c.Mailbox(); mbox != nil && mbox.Name == f.Name {
		return nil
	}
	_, err = c.Select(f.Name, nil).Wait()
	return err
}

func (a *account) steadyWithBudget(ctx context.Context, c *imapclient.Client, caps imap.CapSet, budget time.Duration) error {
	idleOK := caps.Has(imap.CapIdle)
	// IDLE needs an inbox to watch. Accounts without one still need poll cycles
	// to finish a budgeted reconnect pass.
	if inbox, err := a.m.st.FolderBySpecial(a.cfg.Name, "inbox"); err != nil {
		return err
	} else if inbox == nil {
		idleOK = false
	}
	// An unfinished reconnect pass is resumed after the normal short delay.
	sync := !a.sweep.reconnect
	for {
		if _, err := a.drain(c); err != nil {
			return err
		}
		// A trip round this loop is a sync only when something asked for one:
		// IDLE announced data, the poll elapsed, someone hit refresh, or a queued
		// command changed mailbox state. Say so before doing the work, not just
		// "ok" after — without this the steady state (i.e. nearly every
		// background sync) went ok -> ok and broadcast a single event, which is
		// why the spinner could only ever blink. Read-only commands skip the
		// whole block: they can't change the mailbox, so there is nothing to sync
		// back, and a body open or a server-side search has no business flipping
		// the account to "syncing" or dragging a full inbox sync in behind it.
		if sync {
			a.setStatus("syncing", "")
			// Retry any \Seen flip that never reached the server before syncing
			// flags back from it — otherwise the sync reads a stale "unread" and
			// the local mark-read is lost.
			a.pushSeenDirty(c)
			// Same debt, different table: a draft saved while the server was
			// unreachable (or before it was an IMAP thing at all — migration
			// 0230) is published here, so it reaches the user's other clients
			// without them having to open compose again.
			a.pushDirtyDrafts(ctx, c)
			// Re-read the set every cycle, like the poll interval below: ticking a
			// folder in Settings → Syncing takes effect on the next cycle, without
			// a reconnect.
			var changed bool
			var resume string
			var err error
			if a.sweep.reconnect {
				var folders []store.Folder
				folders, err = a.m.st.ListFolders(a.cfg.Name)
				if err == nil {
					changed, resume, err = a.sweepFolderSet(ctx, c, caps, folders, a.sweep.resume, budget)
				}
			} else {
				changed, resume, err = a.sweepFolders(ctx, c, caps, a.sweep.resume, budget)
			}
			if err != nil {
				return err
			}
			a.sweep.resume = resume
			if a.sweep.reconnect {
				a.sweep.reconnect = resume != ""
			}
			if changed {
				a.signalListChanged() // one per cycle, whatever changed where
			}
			a.setStatus("ok", "")
		}
		// Re-read the poll interval each cycle so the "Check every N minutes"
		// setting takes effect without a restart.
		poll := a.pollInterval()
		// A sweep that ran out of budget still has the rest of the set to visit:
		// come back for it soon instead of waiting out a whole poll interval.
		// It waits like any other cycle rather than sleeping the gap out, which
		// is what keeps a queued command prompt — and keeps a read-only one from
		// dragging a sync in behind it (see submitRO).
		if a.sweep.resume != "" {
			poll = sweepResumeDelay
		}
		if ctx.Err() != nil {
			return nil
		}
		if idleOK {
			if err := a.selectInbox(c); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			var err error
			if sync, err = a.waitIdle(ctx, c, poll); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		} else {
			sync = a.waitWork(ctx, poll)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// sweepBudget bounds one steady-state sweep. A sweep that runs longer than this
// stops between folders and the next cycle picks up where it left off, so the
// account reports "ok" on a schedule instead of sitting on "syncing" for as long
// as its biggest mailbox takes. It is a backstop, not the fix: the per-folder
// work in syncFolder is what makes a sweep fit in the budget on a big account.
const sweepBudget = 20 * time.Second

// sweepResumeDelay is the poll a cycle uses while a sweep still has folders to
// visit. Short enough that the rotation keeps moving on an account whose set
// cannot fit in one budget, long enough that the status badge spends real time on
// "ok" between passes rather than blinking on a loop.
const sweepResumeDelay = 5 * time.Second

// sweepFolders walks the account's synced set for one cycle. It is what the
// steady loop calls instead of looping over syncedFolders itself, for two
// reasons that are the same reason:
//
//   - it yields to the command queue between folders (syncFolder yields inside
//     one), so an interactive command — a body open, a server-side search —
//     never waits out the whole sweep. It used to wait for all of it, then fail
//     at submitTimeout, which is what left the reading pane on its skeleton;
//   - it stops at the cycle's budget and reports where to resume, so the sweep
//     is bounded no matter how many folders the account exposes or how large
//     they are.
//
// The inbox is swept first, ahead of the rotation, and every cycle: it is the
// folder whose freshness anyone notices, and once a sweep can be cut short,
// "last in the stored order" would mean "last in a rotation several cycles
// long". Ending the cycle on the inbox is no longer needed either — selectInbox
// is what puts IDLE back on the mailbox new mail arrives in.
//
// from is the resume point ("" = start at the top); the returned resume is ""
// once the whole set has been visited.
func (a *account) sweepFolders(ctx context.Context, c *imapclient.Client, caps imap.CapSet, from string, budget time.Duration) (changed bool, resume string, err error) {
	folders, err := a.syncedFolders()
	if err != nil {
		return false, "", err
	}
	return a.sweepFolderSet(ctx, c, caps, folders, from, budget)
}

func (a *account) sweepFolderSet(ctx context.Context, c *imapclient.Client, caps imap.CapSet, folders []store.Folder, from string, budget time.Duration) (changed bool, resume string, err error) {
	deadline := time.Now().Add(budget)

	var rest []store.Folder
	for i := range folders {
		if folders[i].SpecialUse == "inbox" {
			if _, err := a.yield(ctx, c); err != nil {
				return changed, "", err
			}
			got, err := a.syncFolder(ctx, c, &folders[i], caps)
			if err != nil {
				return changed, "", err
			}
			changed = changed || got
			continue
		}
		rest = append(rest, folders[i])
	}

	start := 0
	if from != "" {
		// A folder that is gone from the set (deselected, renamed) drops the
		// resume point rather than the sweep: start over.
		for i := range rest {
			if rest[i].Name == from {
				start = i
				break
			}
		}
	}
	for i := start; i < len(rest); i++ {
		if _, err := a.yield(ctx, c); err != nil {
			return changed, "", err
		}
		got, err := a.syncFolder(ctx, c, &rest[i], caps)
		if err != nil {
			return changed, "", err
		}
		changed = changed || got
		// The budget is checked between folders, never inside one: a folder is
		// the unit of work here, and the yields inside syncFolder are what keep a
		// queued command from waiting on it.
		if time.Now().After(deadline) && i+1 < len(rest) {
			// Park on the inbox before handing the rest to the next cycle: the
			// sweep is what moved the connection off it, and everything after it —
			// IDLE, and the reconnect path that mirrors it — waits on the mailbox
			// new mail arrives in. Cheap and idempotent: selectInbox is a local
			// row read and a name compare when nothing moved.
			if err := a.selectInbox(c); err != nil {
				return changed, "", err
			}
			return changed, rest[i+1].Name, nil
		}
	}
	if err := a.selectInbox(c); err != nil {
		return changed, "", err
	}
	return changed, "", nil
}

// yield gives the command queue a turn in the middle of a sweep. It is drain
// plus the context check the sweeps would otherwise do at their next folder
// boundary: an interactive command queued while the worker is inside a sync gets
// the connection now — between a folder's fetch and its reconciliation, or
// between two folders — instead of at the end of the sweep.
//
// Ordering is preserved and nothing is added to the connection model: the
// command still runs on the worker's own connection (see submitRO), which is why
// this is safe on providers that cap concurrent connections per account.
//
// It reports whether it served a command, because a served command SELECTs its
// own mailbox on this connection and leaves it selected. A yield between folders
// does not care — the next syncFolder SELECTs. A yield *inside* a pass does: see
// the re-SELECT in syncFolder.
func (a *account) yield(ctx context.Context, c *imapclient.Client) (served bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return a.drain(c)
}

// syncSettings resolves this account's effective sync-interval/max-per-sync/
// sync-months/body-cache: its own override where set (Settings → Syncing,
// per-account section), else the global Prefs value.
func (a *account) syncSettings() (intervalMin, maxPerSync, syncMonths, bodyCache int) {
	return store.EffectiveSyncSettings(a.m.st.GetPrefs(), a.cfg)
}

// pollInterval is the effective sync cadence (Settings → "Check every N
// minutes", account override or global), falling back to a 5-minute default.
func (a *account) pollInterval() time.Duration {
	if n, _, _, _ := a.syncSettings(); n > 0 {
		return time.Duration(n) * time.Minute
	}
	return config.DefaultPollInterval
}

// waitIdle blocks in IMAP IDLE until new data arrives, a command is queued, the
// context ends, or the poll interval elapses (capped at the ~29-minute server
// limit) so the "Check every N minutes" setting still applies under IDLE. It
// reports whether the wake-up calls for a sync; a read-only command doesn't, and
// leaves any pending wake token in place so a sync that was also due still runs
// on the next trip.
func (a *account) waitIdle(ctx context.Context, c *imapclient.Client, poll time.Duration) (sync bool, err error) {
	idleCmd, err := c.Idle()
	if err != nil {
		return false, err
	}
	if poll <= 0 || poll > 28*time.Minute {
		poll = 28 * time.Minute
	}
	sync = a.waitWork(ctx, poll)
	if err := idleCmd.Close(); err != nil {
		return sync, err
	}
	return sync, idleCmd.Wait()
}

// waitWork blocks until the worker has something to do, and reports whether it
// calls for a sync. This is the whole of requirement (1): everything wakes the
// loop, but only new data, the elapsed poll, an explicit refresh or a queued
// state-changing command asks it to sync. A read-only command arrives on nudge
// and gets the connection without a sync or a "syncing" status.
func (a *account) waitWork(ctx context.Context, poll time.Duration) (sync bool) {
	select {
	case <-ctx.Done():
	case <-a.nudge:
	case <-a.wake:
		sync = true
	case <-time.After(poll):
		sync = true
	}
	return sync
}

// drain runs every queued command against the connection, reporting each
// result to its caller. served says whether it ran anything: the commands it
// drains SELECT their own mailbox on this connection and leave it selected
// (TestSelectInboxAfterReadOnlyCommand), which a caller about to do UID work on
// a folder of its own has to account for.
func (a *account) drain(c *imapclient.Client) (served bool, err error) {
	for {
		select {
		case cm := <-a.cmds:
			cm.done <- cm.fn(c)
			served = true
		default:
			return served, nil
		}
	}
}

// submit queues a state-changing command (STORE, MOVE, APPEND) for the worker
// and waits for its result. It asks for a sync too: the command just changed the
// mailbox, so the loop should read the change back and announce it.
func (a *account) submit(ctx context.Context, fn func(*imapclient.Client) error) error {
	return a.enqueue(ctx, fn, true)
}

// submitRO queues a read-only command — SEARCH or FETCH under a read-only
// SELECT (search.go, attachments.go, fetchRaw). It breaks IDLE so the command
// runs now and nothing more: a command that cannot change the mailbox has
// nothing to sync back, so it must not flip the account to "syncing" nor drag a
// full inbox sync in behind it. Opening one uncached body used to do both, on
// every account. Keeping "mutates" the default of the shorter name means a
// future caller that never thinks about this gets the safe, old behaviour.
func (a *account) submitRO(ctx context.Context, fn func(*imapclient.Client) error) error {
	return a.enqueue(ctx, fn, false)
}

// exec runs an IMAP command on a connection the caller already holds, and falls
// back to the worker's queue (nil c) for everyone who holds none — HTTP
// handlers, the scheduler. The distinction is not an optimisation: filter rules
// run from inside fetchSet, i.e. on the worker goroutine itself, and drain() is
// the only thing that ever empties a.cmds. A rule action that called submit
// there would wait on a queue only the goroutine it is blocking can drain — a
// full submitTimeout of stalled sync, then a failed action.
func (a *account) exec(ctx context.Context, c *imapclient.Client, fn func(*imapclient.Client) error) error {
	if c != nil {
		return fn(c)
	}
	return a.submit(ctx, fn)
}

// enqueue queues a command for the worker and waits for its result. Bounded by
// submitTimeout so a wedged/unreachable server (worker stuck in connect/backoff,
// never draining a.cmds) fails the caller instead of hanging on ctx forever —
// same budget as background()'s post-request IMAP timeout in server/mail.go.
// cm.done is buffered, so if the worker completes after we've given up, its
// send doesn't block and the goroutine still exits.
func (a *account) enqueue(ctx context.Context, fn func(*imapclient.Client) error, mutates bool) error {
	ctx, cancel := context.WithTimeout(ctx, submitTimeout)
	defer cancel()
	cm := cmd{fn: fn, done: make(chan error, 1)}
	select {
	case a.cmds <- cm:
		if mutates {
			a.signalWake()
		} else {
			a.signalNudge()
		}
	case <-ctx.Done():
		return busyErr(ctx.Err())
	}
	select {
	case err := <-cm.done:
		return err
	case <-ctx.Done():
		return busyErr(ctx.Err())
	}
}

// busyErr turns the caller's deadline into the answer the UI can explain. The
// budget is submitTimeout, and the only way to spend it is the worker not getting
// to the queue — a sweep that is still running, or a connection stuck in
// backoff. "Busy" is the honest description of the first, which is the one a
// user actually hits: opening a message while a big account syncs. A cancelled
// request (the browser navigated away) is left as it is.
func busyErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrBusy, err)
	}
	return err
}

// submitTimeout bounds a queued IMAP command end-to-end (enqueue + execution).
const submitTimeout = 30 * time.Second

// ErrBusy is what a queued command reports when it ran out of submitTimeout
// without the worker ever reaching it — the account is mid-sweep (or wedged in
// connect-backoff), not broken. Callers that talk to a user (the reading pane)
// use it to say something true instead of "could not load".
var ErrBusy = errors.New("the account is busy syncing")

// FetchNotice is the sentence a caller reports when a fetch for one part of a
// message failed — the reading pane, the HTTP API, the MCP tools. what names the
// part ("body", "headers", "raw message", "attachments").
//
// The busy case is the one worth naming, and it is the reason this lives next to
// ErrBusy rather than in each surface: a fetch that ran out of submitTimeout
// queued behind a sync sweep is not an offline account, and it is the failure a
// user with a large mailbox meets most often. Every surface that reports a fetch
// failure says it the same way, so none of them tells the network story for a
// queue problem.
func FetchNotice(what string, err error) string {
	if errors.Is(err, ErrBusy) {
		return "This account is busy syncing, so the " + what + " could not be loaded. Try again in a moment."
	}
	return "Couldn't fetch the " + what + " — the account may be offline."
}

// sleepOrWake waits out the connect backoff, but cuts it short when someone asks
// for a refresh, so an explicit refresh of a broken account retries now instead
// of sitting out up to two minutes. It also consumes the wake token that
// RefreshAll queued — a worker parked in a plain sleep() never did, so the token
// survived into the next session and bought one off-cadence sync.
func (a *account) sleepOrWake(ctx context.Context, d time.Duration) (cancelled bool) {
	select {
	case <-ctx.Done():
		return true
	case <-a.wake:
		return false
	case <-time.After(d):
		return false
	}
}

func sleep(ctx context.Context, d time.Duration) (cancelled bool) {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 2*time.Minute {
		return 2 * time.Minute
	}
	return d
}
