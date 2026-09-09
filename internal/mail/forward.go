// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"fmt"
	"github.com/mattmezza/mimux/internal/store"
)

// ForwardAttachments validates and fetches original attachments for web and automation sends.
func (m *Manager) ForwardAttachments(ctx context.Context, account string, sourceID int64, selected []store.ForwardAttachment, existing []OutAttachment) ([]OutAttachment, string) {
	msg, err := m.st.MessageByID(sourceID)
	if err != nil || msg == nil || msg.Account != account {
		return nil, "Could not find the original message's attachments. Your draft is safe."
	}
	available, err := m.Attachments(ctx, msg)
	if err != nil {
		return nil, "Could not check the original attachments. Check your connection and try again — your draft is safe."
	}
	var total int64
	for _, a := range existing {
		total += int64(len(a.Data))
	}
	out := make([]OutAttachment, 0, len(selected))
	for _, wanted := range selected {
		var found *Attachment
		for i := range available {
			if intsEqual(available[i].Part, wanted.Part) {
				found = &available[i]
				break
			}
		}
		if found == nil {
			return nil, fmt.Sprintf("The original attachment %q is no longer available. Remove it or try again — your draft is safe.", wanted.Filename)
		}
		// BODYSTRUCTURE size is available before the part bytes are requested.
		// Reject known-oversized selections here so a hostile or accidental huge
		// attachment cannot be buffered into process memory merely to discover it
		// exceeds the compose limit. Keep the actual decoded-size check below too.
		if found.Size > 0 && total+int64(found.Size) > MaxAttachTotal {
			return nil, fmt.Sprintf("Attachments exceed the %dMB limit. Remove %q and try again — your draft is safe.", MaxAttachTotal>>20, found.Filename)
		}
		data, mediaType, filename, fetchErr := m.Attachment(ctx, msg, found.Part)
		if fetchErr != nil {
			return nil, fmt.Sprintf("Could not fetch the original attachment %q. Try again — your draft is safe.", found.Filename)
		}
		total += int64(len(data))
		if total > MaxAttachTotal {
			return nil, fmt.Sprintf("Attachments exceed the %dMB limit. Your draft is safe.", MaxAttachTotal>>20)
		}
		if filename == "" {
			filename = found.Filename
		}
		if mediaType == "" {
			mediaType = found.MediaType
		}
		out = append(out, OutAttachment{Filename: filename, ContentType: mediaType, Data: data})
	}
	return out, ""
}

// RawAttachment reads a message for attachment creation, enforcing the same
// limit before requesting known-large messages and after reading actual bytes.
func (m *Manager) RawAttachment(ctx context.Context, msg *store.Message) ([]byte, error) {
	if msg.Size > MaxAttachTotal {
		return nil, fmt.Errorf("message exceeds the %dMB attachment limit", MaxAttachTotal>>20)
	}
	raw, err := m.Raw(ctx, msg)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxAttachTotal {
		return nil, fmt.Errorf("message exceeds the %dMB attachment limit", MaxAttachTotal>>20)
	}
	return raw, nil
}

// ValidateAttachmentSize caps the combined bytes from saved, fresh and forwarded files.
func ValidateAttachmentSize(atts []OutAttachment) error {
	var total int64
	for _, at := range atts {
		total += int64(len(at.Data))
		if total > MaxAttachTotal {
			return fmt.Errorf("attachments exceed the %dMB limit", MaxAttachTotal>>20)
		}
	}
	return nil
}
