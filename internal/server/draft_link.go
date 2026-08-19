// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"strconv"

	"github.com/mattmezza/mimux/internal/store"
)

// draftLinks maps the id of every message in msgs that sits in its account's
// Drafts folder to the compose edit link the drafts page (draftRows) would
// give it — same local-vs-foreign split, so a draft surfacing inside a THREAD
// (not just on /drafts) opens the identical editor. A message id missing from
// the result is not a draft.
//
// Edit link shape mirrors draftRows/drafts.html exactly:
//   - a message with a matching local draft row (DraftByServerCopy) →
//     /compose?draft=<local id>, the same edit-first link a local draft gets.
//   - otherwise → /compose?adopt=<message id>, which adopts it into a local
//     draft on click; a draft mimux can't reproduce (encrypted, signed,
//     inline images) fails there with a toast, same as /drafts today — no
//     separate "unadoptable" detection needed here, the fallback is already
//     read-only (the row itself, not this link).
func (s *Server) draftLinks(msgs []store.Message) map[int64]string {
	draftFolder := map[string]int64{} // account -> Drafts folder id, cached per call
	links := map[int64]string{}
	for i := range msgs {
		m := &msgs[i]
		fid, seen := draftFolder[m.Account]
		if !seen {
			fid = 0
			if f, err := s.store.FolderBySpecial(m.Account, "drafts"); err == nil && f != nil {
				fid = f.ID
			}
			draftFolder[m.Account] = fid
		}
		if fid == 0 || m.FolderID != fid {
			continue
		}
		if d, err := s.store.DraftByServerCopy(m.Account, m.MessageID, m.FolderID, m.UID); err == nil && d != nil {
			links[m.ID] = "/compose?draft=" + strconv.FormatInt(d.ID, 10)
			continue
		}
		links[m.ID] = "/compose?adopt=" + strconv.FormatInt(m.ID, 10)
	}
	return links
}
