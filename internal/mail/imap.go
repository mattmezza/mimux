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
	Type string // "sync-status" | "new-mail"
	Data string // account name
}

// AccountStatus is the live sync state surfaced in the status bar.
type AccountStatus struct {
	Account string
	State   string // "ok" | "syncing" | "error"
	Message string
}

// Manager owns one worker per configured account and a shared body cache.
type Manager struct {
	cfg      *config.Config
	st       *store.Store
	bodies   *bodyLRU
	hub      *hub
	accounts map[string]*account
}

func NewManager(cfg *config.Config, st *store.Store) *Manager {
	m := &Manager{
		cfg:      cfg,
		st:       st,
		bodies:   newBodyLRU(32),
		hub:      newHub(),
		accounts: map[string]*account{},
	}
	for _, ac := range cfg.Accounts {
		m.accounts[ac.Name] = &account{
			cfg:    ac,
			m:      m,
			cmds:   make(chan cmd, 64),
			wake:   make(chan struct{}, 1),
			status: AccountStatus{Account: ac.Name, State: "syncing"},
		}
	}
	return m
}

// Start launches every account worker; they stop when ctx is cancelled.
func (m *Manager) Start(ctx context.Context) {
	for _, a := range m.accounts {
		go a.run(ctx)
	}
}

// Status returns a snapshot of every account's sync state, sorted by name.
func (m *Manager) Status() []AccountStatus {
	out := make([]AccountStatus, 0, len(m.accounts))
	for _, a := range m.accounts {
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
	if a := m.accounts[accountName]; a != nil {
		a.signalWake()
	}
}

// RefreshAll nudges every account worker to sync now (the "Refresh now" button).
func (m *Manager) RefreshAll() {
	for _, a := range m.accounts {
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
	wake chan struct{}

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
	a.status = AccountStatus{Account: a.cfg.Name, State: state, Message: msg}
	a.mu.Unlock()
	a.m.hub.broadcast(Event{Type: "sync-status", Data: a.cfg.Name})
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
	for i := range folders {
		if ctx.Err() != nil {
			return nil
		}
		if err := a.syncFolder(ctx, c, &folders[i], caps); err != nil {
			return err
		}
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
		if err := a.syncFolder(ctx, c, inbox, caps); err != nil {
			return err
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
// minutes"), falling back to the config file / a 5-minute default.
func (a *account) pollInterval() time.Duration {
	if n := a.m.st.GetPrefs().SyncIntervalMin; n > 0 {
		return time.Duration(n) * time.Minute
	}
	if d := a.m.cfg.Sync.Interval(); d > 0 {
		return d
	}
	return 5 * time.Minute
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

// submit queues a command for the worker and waits for its result.
func (a *account) submit(ctx context.Context, fn func(*imapclient.Client) error) error {
	cm := cmd{fn: fn, done: make(chan error, 1)}
	select {
	case a.cmds <- cm:
		a.signalWake()
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(15 * time.Second):
		return errors.New("account busy")
	}
	select {
	case err := <-cm.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
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
