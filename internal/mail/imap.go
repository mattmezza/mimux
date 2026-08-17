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
	Type string // "sync-status" | "new-mail" (any list change) | "toast" | search-*
	Data string // account name
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

// Subscribe returns an event channel and an unsubscribe func for SSE.
func (m *Manager) Subscribe() (<-chan Event, func()) { return m.hub.subscribe() }

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

// session runs the initial full sync of all folders then the steady-state loop
// on the inbox.
func (a *account) session(ctx context.Context, c *imapclient.Client) error {
	a.setStatus("syncing", "")
	folders, err := a.syncFolders(c)
	if err != nil {
		return err
	}
	caps := c.Caps()
	var inbox *store.Folder
	for i := range folders {
		f := &folders[i]
		if f.SpecialUse == "inbox" {
			inbox = f
		}
	}
	// Inbox first, then the rest.
	sort.SliceStable(folders, func(i, j int) bool {
		return folders[i].SpecialUse == "inbox" && folders[j].SpecialUse != "inbox"
	})
	anyChanged := false
	for i := range folders {
		if ctx.Err() != nil {
			return nil
		}
		changed, err := a.syncFolder(ctx, c, &folders[i], caps)
		if err != nil {
			return err
		}
		anyChanged = anyChanged || changed
	}
	if anyChanged {
		a.signalListChanged()
	}
	a.setStatus("ok", "")
	if inbox == nil {
		// No inbox: just idle-poll nothing; wait for shutdown.
		<-ctx.Done()
		return nil
	}
	return a.steady(ctx, c, inbox, caps)
}

func (a *account) steady(ctx context.Context, c *imapclient.Client, inbox *store.Folder, caps imap.CapSet) error {
	idleOK := caps.Has(imap.CapIdle)
	// The first trip syncs unconditionally, as it always did: session() has just
	// finished the full sweep and this re-reads the inbox before settling in.
	sync := true
	for {
		if err := a.drain(c); err != nil {
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
			changed, err := a.syncFolder(ctx, c, inbox, caps)
			if err != nil {
				return err
			}
			if changed {
				a.signalListChanged()
			}
			a.setStatus("ok", "")
		}
		// Re-read the poll interval each cycle so the "Check every N minutes"
		// setting takes effect without a restart.
		poll := a.pollInterval()
		if idleOK {
			var err error
			if sync, err = a.waitIdle(ctx, c, poll); err != nil {
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
// result to its caller.
func (a *account) drain(c *imapclient.Client) error {
	for {
		select {
		case cm := <-a.cmds:
			cm.done <- cm.fn(c)
		default:
			return nil
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
		return ctx.Err()
	}
	select {
	case err := <-cm.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// submitTimeout bounds a queued IMAP command end-to-end (enqueue + execution).
const submitTimeout = 30 * time.Second

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
