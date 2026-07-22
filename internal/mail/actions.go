package mail

import (
	"context"
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/sm/internal/store"
)

// Body returns the sanitized HTML document for a message, fetching and caching
// the raw body on demand. blockedExternal reports whether external images were
// suppressed.
func (m *Manager) Body(ctx context.Context, msg *store.Message, allowExternal bool) (out string, blockedExternal bool, err error) {
	if !allowExternal {
		if b, ok := m.bodies.get(msg.ID); ok {
			out, blocked := b.render(false)
			return out, blocked, nil
		}
	}
	body, ok := m.bodies.get(msg.ID)
	if !ok {
		raw, err := m.fetchRaw(ctx, msg)
		if err != nil {
			return "", false, err
		}
		body = parseBody(raw)
		m.bodies.put(msg.ID, body)
	}
	out, blocked := body.render(allowExternal)
	return out, blocked, nil
}

func (m *Manager) fetchRaw(ctx context.Context, msg *store.Message) ([]byte, error) {
	a := m.accounts[msg.Account]
	if a == nil {
		return nil, fmt.Errorf("unknown account %q", msg.Account)
	}
	f, err := m.st.FolderByID(msg.FolderID)
	if err != nil || f == nil {
		return nil, fmt.Errorf("folder not found")
	}
	var raw []byte
	err = a.submit(ctx, func(c *imapclient.Client) error {
		if _, err := c.Select(f.Name, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return err
		}
		set := imap.UIDSet{}
		set.AddNum(imap.UID(msg.UID))
		data, err := c.Fetch(set, &imap.FetchOptions{
			BodySection: []*imap.FetchItemBodySection{{Peek: true}},
		}).Collect()
		if err != nil {
			return err
		}
		if len(data) == 0 || len(data[0].BodySection) == 0 {
			return fmt.Errorf("empty body")
		}
		raw = data[0].BodySection[0].Bytes
		return nil
	})
	return raw, err
}

// SetRead adds or removes the \Seen flag on the server.
func (m *Manager) SetRead(ctx context.Context, msg *store.Message, read bool) error {
	return m.storeFlag(ctx, msg, imap.FlagSeen, read)
}

// SetStarred adds or removes the \Flagged flag on the server.
func (m *Manager) SetStarred(ctx context.Context, msg *store.Message, starred bool) error {
	return m.storeFlag(ctx, msg, imap.FlagFlagged, starred)
}

func (m *Manager) storeFlag(ctx context.Context, msg *store.Message, flag imap.Flag, add bool) error {
	a := m.accounts[msg.Account]
	if a == nil {
		return fmt.Errorf("unknown account %q", msg.Account)
	}
	f, err := m.st.FolderByID(msg.FolderID)
	if err != nil || f == nil {
		return fmt.Errorf("folder not found")
	}
	op := imap.StoreFlagsAdd
	if !add {
		op = imap.StoreFlagsDel
	}
	return a.submit(ctx, func(c *imapclient.Client) error {
		if _, err := c.Select(f.Name, nil).Wait(); err != nil {
			return err
		}
		set := imap.UIDSet{}
		set.AddNum(imap.UID(msg.UID))
		return c.Store(set, &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{flag}}, nil).Close()
	})
}

// Move relocates a message to the account's folder with the given special-use
// role (trash/archive/spam), via MOVE or COPY+\Deleted+EXPUNGE fallback.
func (m *Manager) Move(ctx context.Context, msg *store.Message, targetSpecial string) error {
	target, err := m.st.FolderBySpecial(msg.Account, targetSpecial)
	if err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("no %s folder for this account", targetSpecial)
	}
	return m.moveTo(ctx, msg, target)
}

// MoveToFolder is Move's named-folder variant, for filter rule "move"
// actions: name resolves against the account's special-use folders first
// (so "archive"/"trash"/... keep working), then against literal folder
// names.
func (m *Manager) MoveToFolder(ctx context.Context, msg *store.Message, name string) error {
	target, err := m.st.FolderBySpecial(msg.Account, strings.ToLower(name))
	if err != nil {
		return err
	}
	if target == nil {
		if target, err = m.st.FolderByName(msg.Account, name); err != nil {
			return err
		}
	}
	if target == nil {
		return fmt.Errorf("no folder named %q for account %q", name, msg.Account)
	}
	return m.moveTo(ctx, msg, target)
}

func (m *Manager) moveTo(ctx context.Context, msg *store.Message, target *store.Folder) error {
	a := m.accounts[msg.Account]
	if a == nil {
		return fmt.Errorf("unknown account %q", msg.Account)
	}
	src, err := m.st.FolderByID(msg.FolderID)
	if err != nil || src == nil {
		return fmt.Errorf("folder not found")
	}
	if target.ID == src.ID {
		return nil
	}
	return a.submit(ctx, func(c *imapclient.Client) error {
		if _, err := c.Select(src.Name, nil).Wait(); err != nil {
			return err
		}
		set := imap.UIDSet{}
		set.AddNum(imap.UID(msg.UID))
		if c.Caps().Has(imap.CapMove) {
			_, err := c.Move(set, target.Name).Wait()
			return err
		}
		if _, err := c.Copy(set, target.Name).Wait(); err != nil {
			return err
		}
		if err := c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
			return err
		}
		if c.Caps().Has(imap.CapUIDPlus) {
			return c.UIDExpunge(set).Close()
		}
		return c.Expunge().Close()
	})
}

// Toast pushes a transient error message to connected clients.
func (m *Manager) Toast(text string) { m.hub.broadcast(Event{Type: "toast", Data: text}) }
