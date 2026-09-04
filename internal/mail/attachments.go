// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/mimux/internal/store"
)

var unsafeMessageFilename = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)

// MessageFilename turns a subject into a portable .eml filename.
func MessageFilename(subject string, id int64) string {
	name := strings.TrimSpace(unsafeMessageFilename.ReplaceAllString(subject, "_"))
	name = strings.Trim(name, ". ")
	if len([]rune(name)) > 120 {
		name = string([]rune(name)[:120])
	}
	if name == "" {
		name = fmt.Sprintf("message-%d", id)
	}
	return name + ".eml"
}

// Attachment is one downloadable/previewable MIME part of a message. Part is
// the IMAP body path (e.g. [2] or [1,2]) used to fetch the bytes.
type Attachment struct {
	Part      []int
	Filename  string
	MediaType string
	Size      uint32
}

// isAttachmentPart applies the same predicate as hasAttachment to one part.
func isAttachmentPart(sp *imap.BodyStructureSinglePart) bool {
	if d := sp.Disposition(); d != nil && strings.EqualFold(d.Value, "attachment") {
		return true
	}
	return sp.Filename() != "" && !strings.HasPrefix(sp.MediaType(), "text/")
}

// Attachments lists a message's attachments by fetching its BODYSTRUCTURE live
// (nothing is persisted beyond the HasAttachment flag).
func (m *Manager) Attachments(ctx context.Context, msg *store.Message) ([]Attachment, error) {
	a := m.accounts[msg.Account]
	if a == nil {
		return nil, fmt.Errorf("unknown account %q", msg.Account)
	}
	f, err := m.st.FolderByID(msg.FolderID)
	if err != nil || f == nil {
		return nil, fmt.Errorf("folder not found")
	}
	var out []Attachment
	err = a.submitRO(ctx, func(c *imapclient.Client) error {
		if _, err := c.Select(f.Name, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return err
		}
		set := imap.UIDSet{}
		set.AddNum(imap.UID(msg.UID))
		data, err := c.Fetch(set, &imap.FetchOptions{
			BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		}).Collect()
		if err != nil {
			return err
		}
		if len(data) == 0 || data[0].BodyStructure == nil {
			return nil
		}
		data[0].BodyStructure.Walk(func(path []int, part imap.BodyStructure) bool {
			sp, ok := part.(*imap.BodyStructureSinglePart)
			if !ok || !isAttachmentPart(sp) {
				return true
			}
			name := sp.Filename()
			if name == "" {
				name = "attachment"
			}
			out = append(out, Attachment{
				Part:      append([]int(nil), path...),
				Filename:  name,
				MediaType: sp.MediaType(),
				Size:      sp.Size,
			})
			return true
		})
		return nil
	})
	return out, err
}

// Attachment fetches and transfer-decodes a single MIME part's bytes, live from
// IMAP. Returns the decoded content plus its media type and filename.
func (m *Manager) Attachment(ctx context.Context, msg *store.Message, part []int) (data []byte, mediaType, filename string, err error) {
	a := m.accounts[msg.Account]
	if a == nil {
		return nil, "", "", fmt.Errorf("unknown account %q", msg.Account)
	}
	f, ferr := m.st.FolderByID(msg.FolderID)
	if ferr != nil || f == nil {
		return nil, "", "", fmt.Errorf("folder not found")
	}
	sec := &imap.FetchItemBodySection{Part: part, Peek: true}
	err = a.submitRO(ctx, func(c *imapclient.Client) error {
		if _, e := c.Select(f.Name, &imap.SelectOptions{ReadOnly: true}).Wait(); e != nil {
			return e
		}
		set := imap.UIDSet{}
		set.AddNum(imap.UID(msg.UID))
		d, e := c.Fetch(set, &imap.FetchOptions{
			BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
			BodySection:   []*imap.FetchItemBodySection{sec},
		}).Collect()
		if e != nil {
			return e
		}
		if len(d) == 0 || len(d[0].BodySection) == 0 {
			return fmt.Errorf("attachment part not found")
		}
		enc := ""
		if d[0].BodyStructure != nil {
			d[0].BodyStructure.Walk(func(p []int, pt imap.BodyStructure) bool {
				if sp, ok := pt.(*imap.BodyStructureSinglePart); ok && intsEqual(p, part) {
					enc, mediaType, filename = sp.Encoding, sp.MediaType(), sp.Filename()
				}
				return true
			})
		}
		data = decodeTransfer(d[0].BodySection[0].Bytes, enc)
		return nil
	})
	return data, mediaType, filename, err
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
