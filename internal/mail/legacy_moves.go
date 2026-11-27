// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/mimux/internal/store"
)

// Old optimistic moves could leave the source UID recorded in the destination.
// Verify the Message-ID as well as the UID before treating such a row as live.
func (a *account) reconcileLegacyMoves(c *imapclient.Client, f *store.Folder) (changed bool, err error) {
	pending, err := a.m.st.LegacyPendingMessages(f.ID)
	if err != nil {
		return false, err
	}
	for start := 0; start < len(pending); start += deepBatchSize {
		batch := pending[start:min(start+deepBatchSize, len(pending))]
		var set imap.UIDSet
		for _, msg := range batch {
			set.AddNum(imap.UID(msg.UID))
		}
		found, err := c.Fetch(set, &imap.FetchOptions{UID: true, Envelope: true}).Collect()
		if err != nil {
			return changed, err
		}
		live := make(map[uint32]string, len(found))
		for _, buf := range found {
			var messageID string
			if buf.Envelope != nil {
				messageID = buf.Envelope.MessageID
			}
			live[uint32(buf.UID)] = messageID
		}
		for _, msg := range batch {
			messageID, exists := live[msg.UID]
			if exists && (messageID == "" || msg.MessageID == "") {
				continue
			}
			keep := exists && messageID == msg.MessageID
			if err := a.m.st.ResolveLegacyPendingMessage(msg.ID, keep); err != nil {
				return changed, err
			}
			changed = changed || !keep
		}
	}
	return changed, nil
}
