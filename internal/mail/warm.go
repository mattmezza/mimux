package mail

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/sm/internal/store"
)

// The warmer is a second IMAP connection per account whose only job is to fetch
// and cache inbox bodies before the user opens them — a cold BODY[] fetch on
// click is what made opening a message slow. It never uses a.submit, so it can
// never queue behind (or ahead of) an interactive command: those keep the
// worker connection to themselves.
//
// It also never touches a.setStatus: account health in the status bar is the
// interactive worker's story, and a warmer hiccup must not read as "error".

// NOTE: 200 rows per query, one message per FETCH, inbox only. Collect()
// buffers whole raw messages, so a chunk of N holds N × up to 25MB at once —
// nothing is waiting on this loop, so it trades that spike for round trips.
// Raise warmChunk only with a streaming fetch (FetchCommand.Next) behind it.
const (
	warmBatch    = 200
	warmChunk    = 1
	warmChunkGap = 100 * time.Millisecond
)

func (a *account) signalWarm() {
	select {
	case a.warm <- struct{}{}:
	default:
	}
}

// runWarmer is the warmer's connect/backoff supervisor, mirroring run but
// logging instead of reporting status.
func (a *account) runWarmer(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		c, err := a.connect()
		if err != nil {
			if errors.Is(err, ErrNoToken) {
				// Park until the OAuth callback (Manager.Wake) or a sync signals us.
				select {
				case <-ctx.Done():
					return
				case <-a.warm:
				}
				continue
			}
			slog.Debug("warmer connect failed", "account", a.cfg.Name, "err", err)
			if sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = time.Second
		err = a.warmSession(ctx, c)
		_ = c.Logout()
		_ = c.Close()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("warmer session ended", "account", a.cfg.Name, "err", err)
		}
		if sleep(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// warmSession warms once on connect, then on every signal until ctx ends.
func (a *account) warmSession(ctx context.Context, c *imapclient.Client) error {
	for {
		if err := a.warmPass(ctx, c); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-a.warm:
		}
	}
}

// warmPass caches every uncached inbox body it can, a batch at a time. It stops
// when a batch caches nothing new, so messages that never resolve (expunged
// UIDs, unparseable bodies) can't spin the loop.
func (a *account) warmPass(ctx context.Context, c *imapclient.Client) error {
	inbox, err := a.m.st.FolderBySpecial(a.cfg.Name, "inbox")
	if err != nil || inbox == nil {
		return nil // not synced yet: the first sync will signal us
	}
	selected := false
	for ctx.Err() == nil {
		msgs, err := a.m.st.MessagesWithoutBody(inbox.ID, warmBatch)
		if err != nil || len(msgs) == 0 {
			return nil
		}
		if !selected {
			if _, err := c.Select(inbox.Name, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
				return err
			}
			selected = true
		}
		saved := 0
		for i := 0; i < len(msgs); i += warmChunk {
			if ctx.Err() != nil {
				return nil
			}
			end := min(i+warmChunk, len(msgs))
			n, err := a.warmFetch(c, msgs[i:end])
			if err != nil {
				return err
			}
			saved += n
			if sleep(ctx, warmChunkGap) { // stay polite between commands
				return nil
			}
		}
		if saved == 0 {
			return nil
		}
	}
	return nil
}

// warmFetch fetches and caches one chunk of bodies, returning how many it
// stored. UIDs the server no longer has are simply absent from the response.
func (a *account) warmFetch(c *imapclient.Client, msgs []store.Message) (int, error) {
	set := imap.UIDSet{}
	ids := make(map[imap.UID]int64, len(msgs))
	for i := range msgs {
		uid := imap.UID(msgs[i].UID)
		set.AddNum(uid)
		ids[uid] = msgs[i].ID
	}
	data, err := c.Fetch(set, &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil {
		return 0, err
	}
	saved := 0
	for _, buf := range data {
		id, ok := ids[buf.UID]
		if !ok || len(buf.BodySection) == 0 {
			continue
		}
		blob, err := encodeBody(parseBody(buf.BodySection[0].Bytes))
		if err != nil {
			continue
		}
		if err := a.m.st.SaveMessageBody(id, blob); err != nil {
			continue
		}
		saved++
		// Deliberately NOT m.bodies.put: the LRU holds 32 entries and is shared
		// with the reading pane, so warming it would evict what the user is
		// actually reading. The SQLite cache is what makes the open fast.
	}
	return saved, nil
}
