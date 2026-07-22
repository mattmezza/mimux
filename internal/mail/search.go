package mail

import (
	"context"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/sm/internal/search"
	"github.com/mattmezza/sm/internal/store"
)

// Criteria maps a parsed SearchQuery to IMAP SEARCH criteria. in:/label: are
// handled by folder selection, not criteria, and are ignored here.
//
// NOTE: go-imap/v2 beta.8 has no X-GM-RAW support, so Gmail accounts use
// the same standard criteria as everyone else; has:attachment / has:link have
// no portable IMAP key and are dropped from the server query (local search
// still applies them). Upgrade path: switch on account provider + raw command
// once the library exposes extended SEARCH keys.
func Criteria(q *search.SearchQuery) *imap.SearchCriteria {
	c := &imap.SearchCriteria{}
	for _, t := range q.Terms {
		sub := imap.SearchCriteria{}
		switch t.Op {
		case "from":
			sub.Header = []imap.SearchCriteriaHeaderField{{Key: "FROM", Value: t.Value}}
		case "to":
			sub.Header = []imap.SearchCriteriaHeaderField{{Key: "TO", Value: t.Value}}
		case "cc":
			sub.Header = []imap.SearchCriteriaHeaderField{{Key: "CC", Value: t.Value}}
		case "subject":
			sub.Header = []imap.SearchCriteriaHeaderField{{Key: "SUBJECT", Value: t.Value}}
		case "body":
			sub.Body = []string{t.Value}
		case "text":
			sub.Text = []string{t.Value}
		case "label":
			sub.Header = []imap.SearchCriteriaHeaderField{{Key: "X-GM-LABELS", Value: t.Value}}
		case "is":
			switch t.Value {
			case "unread":
				sub.NotFlag = []imap.Flag{imap.FlagSeen}
			case "read":
				sub.Flag = []imap.Flag{imap.FlagSeen}
			case "starred":
				sub.Flag = []imap.Flag{imap.FlagFlagged}
			}
		case "before":
			sub.Before = t.Date
		case "after":
			sub.Since = t.Date
		case "larger":
			sub.Larger = t.Num
		case "smaller":
			sub.Smaller = t.Num
		default:
			continue // has/in and anything else: no portable IMAP key
		}
		if t.Neg {
			c.Not = append(c.Not, sub)
		} else {
			c.And(&sub)
		}
	}
	return c
}

// SearchFolder runs an IMAP UID SEARCH in one folder and returns the matching
// UIDs. It goes through the account worker's command channel, so at most one
// SEARCH per account runs at a time.
func (m *Manager) SearchFolder(ctx context.Context, account string, folder *store.Folder, crit *imap.SearchCriteria) ([]uint32, error) {
	a := m.accounts[account]
	if a == nil {
		return nil, nil
	}
	var uids []uint32
	err := a.submit(ctx, func(c *imapclient.Client) error {
		if _, err := c.Select(folder.Name, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return err
		}
		data, err := c.UIDSearch(crit, nil).Wait()
		if err != nil {
			return err
		}
		for _, u := range data.AllUIDs() {
			uids = append(uids, uint32(u))
		}
		return nil
	})
	return uids, err
}

// CacheEnvelopes fetches envelopes for the given UIDs that aren't already
// cached locally and upserts them, so server-search hits are renderable. It
// returns the message ids (cached + newly fetched), preserving uids order.
func (m *Manager) CacheEnvelopes(ctx context.Context, account string, folder *store.Folder, uids []uint32) ([]int64, error) {
	var missing []uint32
	for _, u := range uids {
		msg, _ := m.st.MessageByFolderUID(folder.ID, u)
		if msg == nil {
			missing = append(missing, u)
		}
	}
	if len(missing) > 0 {
		a := m.accounts[account]
		if a != nil {
			_ = a.submit(ctx, func(c *imapclient.Client) error {
				if _, err := c.Select(folder.Name, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
					return err
				}
				set := imap.UIDSet{}
				for _, u := range missing {
					set.AddNum(imap.UID(u))
				}
				opts := &imap.FetchOptions{
					UID: true, Flags: true, Envelope: true, RFC822Size: true,
					BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
					BodySection:   []*imap.FetchItemBodySection{snippetSection, refsHeaderSection},
				}
				buffers, err := c.Fetch(set, opts).Collect()
				if err != nil {
					return err
				}
				for _, buf := range buffers {
					if buf.Envelope == nil {
						continue
					}
					_ = m.st.UpsertMessage(messageFromBuffer(account, folder.ID, buf))
				}
				return nil
			})
		}
	}
	var ids []int64
	for _, u := range uids {
		if msg, _ := m.st.MessageByFolderUID(folder.ID, u); msg != nil {
			ids = append(ids, msg.ID)
		}
	}
	return ids, nil
}

// Broadcast pushes an arbitrary event to SSE subscribers (used by the search
// orchestration in the server package).
func (m *Manager) Broadcast(eventType, data string) {
	m.hub.broadcast(Event{Type: eventType, Data: data})
}

// SearchTimeout bounds each account's server search.
const SearchTimeout = 10 * time.Second

// SearchFolders returns the folders to search for an account under a scope,
// skipping Trash/Spam unless the query names them via in:. For folder scope the
// caller passes the single current folder directly, so this covers account/all.
//
// NOTE: "all mail"/archive is included; only trash+spam are skipped by
// default, matching what people expect from a mail search.
func (m *Manager) SearchFolders(account string, q *search.SearchQuery) []store.Folder {
	folders, _ := m.st.ListFolders(account)
	named := map[string]bool{}
	for _, t := range q.Terms {
		if t.Op == "in" {
			named[t.Value] = true
		}
	}
	var out []store.Folder
	for _, f := range folders {
		if (f.SpecialUse == "trash" || f.SpecialUse == "spam") && !named[f.SpecialUse] {
			continue
		}
		out = append(out, f)
	}
	return out
}

