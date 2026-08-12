// Package mail implements the IMAP sync engine, body fetching and HTML
// sanitization for SM.
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

	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/store"
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
			cancel: cancel,
			status: AccountStatus{Account: ac.Name, State: "syncing"},
		}
		m.accounts[name] = a
		go a.run(ctx)
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
		len(a.Aliases) != len(b.Aliases) {
		return false
	}
	for i := range a.Aliases {
		if a.Aliases[i] != b.Aliases[i] {
			return false
		}
	}
	return true
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

// Subscribe returns an event channel and an unsubscribe func for SSE.
func (m *Manager) Subscribe() (<-chan Event, func()) { return m.hub.subscribe() }

// Wake nudges an account worker to retry — used after an OAuth authorization
// completes so a worker parked in the "authorize needed" state reconnects.
func (m *Manager) Wake(accountName string) {
	if a := m.account(accountName); a != nil {
		a.signalWake()
	}
}

// RefreshAll nudges every account worker to sync now (the "Refresh now"
// button). It flips each account to "syncing" immediately so the status bar and
// health panel reflect the refresh right away; the worker sets "ok" when done.
func (m *Manager) RefreshAll() {
	m.mu.Lock()
	workers := make([]*account, 0, len(m.accounts))
	for _, a := range m.accounts {
		workers = append(workers, a)
	}
	m.mu.Unlock()
	for _, a := range workers {
		a.setStatus("syncing", "")
		a.signalWake()
	}
}

// --- account worker ---

type cmd struct {
	fn   func(*imapclient.Client) error
	done chan error
}

type account struct {
	cfg    config.Account
	m      *Manager
	cmds   chan cmd
	wake   chan struct{}
	cancel context.CancelFunc // stops this worker (Reload/remove)

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
// client already maps to sm:refresh + an unread-title refresh. Callers coalesce
// to at most one per sync cycle to avoid a refresh storm.
func (a *account) signalListChanged() {
	a.m.hub.broadcast(Event{Type: "new-mail", Data: a.cfg.Name})
}

func (a *account) signalWake() {
	select {
	case a.wake <- struct{}{}:
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
			if sleep(ctx, backoff) {
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
		if sleep(ctx, backoff) {
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
	for {
		if err := a.drain(c); err != nil {
			return err
		}
		changed, err := a.syncFolder(ctx, c, inbox, caps)
		if err != nil {
			return err
		}
		if changed {
			a.signalListChanged()
		}
		a.setStatus("ok", "")
		// Re-read the poll interval each cycle so the "Check every N minutes"
		// setting takes effect without a restart.
		poll := a.pollInterval()
		if idleOK {
			if err := a.waitIdle(ctx, c, poll); err != nil {
				return err
			}
		} else {
			select {
			case <-ctx.Done():
				return nil
			case <-a.wake:
			case <-time.After(poll):
			}
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// pollInterval is the user-configured sync cadence (Settings → "Check every N
// minutes"), falling back to a 5-minute default.
func (a *account) pollInterval() time.Duration {
	if n := a.m.st.GetPrefs().SyncIntervalMin; n > 0 {
		return time.Duration(n) * time.Minute
	}
	return config.DefaultPollInterval
}

// waitIdle blocks in IMAP IDLE until new data arrives, a command is queued, the
// context ends, or the poll interval elapses (capped at the ~29-minute server
// limit) so the "Check every N minutes" setting still applies under IDLE.
func (a *account) waitIdle(ctx context.Context, c *imapclient.Client, poll time.Duration) error {
	idleCmd, err := c.Idle()
	if err != nil {
		return err
	}
	if poll <= 0 || poll > 28*time.Minute {
		poll = 28 * time.Minute
	}
	select {
	case <-ctx.Done():
	case <-a.wake:
	case <-time.After(poll):
	}
	if err := idleCmd.Close(); err != nil {
		return err
	}
	return idleCmd.Wait()
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

// submit queues a command for the worker and waits for its result. Bounded by
// submitTimeout so a wedged/unreachable server (worker stuck in connect/backoff,
// never draining a.cmds) fails the caller instead of hanging on ctx forever —
// same budget as background()'s post-request IMAP timeout in server/mail.go.
// cm.done is buffered, so if the worker completes after we've given up, its
// send doesn't block and the goroutine still exits.
func (a *account) submit(ctx context.Context, fn func(*imapclient.Client) error) error {
	ctx, cancel := context.WithTimeout(ctx, submitTimeout)
	defer cancel()
	cm := cmd{fn: fn, done: make(chan error, 1)}
	select {
	case a.cmds <- cm:
		a.signalWake()
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
