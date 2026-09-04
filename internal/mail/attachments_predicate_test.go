// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func attachmentTestPart(mediaType, name, id, disposition string) *imap.BodyStructureSinglePart {
	typeName, subtype := "application", "octet-stream"
	switch mediaType {
	case "image/png":
		typeName, subtype = "image", "png"
	case "text/plain":
		typeName, subtype = "text", "plain"
	}
	sp := &imap.BodyStructureSinglePart{Type: typeName, Subtype: subtype, Params: map[string]string{"name": name}, ID: id}
	if disposition != "" {
		sp.Extended = &imap.BodyStructureSinglePartExt{Disposition: &imap.BodyStructureDisposition{
			Value: disposition, Params: map[string]string{"filename": name},
		}}
	}
	return sp
}

func TestAttachmentPredicate(t *testing.T) {
	tests := []struct {
		name string
		part *imap.BodyStructureSinglePart
		want bool
	}{
		{"explicit attachment", attachmentTestPart("application/octet-stream", "report.pdf", "", "attachment"), true},
		{"explicit attachment with content id", attachmentTestPart("application/octet-stream", "report.pdf", "invoice@cid", "ATTACHMENT"), true},
		{"inline named image", attachmentTestPart("image/png", "logo.png", "logo@cid", "inline"), false},
		{"content id without disposition", attachmentTestPart("image/png", "footer.png", "footer@cid", ""), false},
		{"named image without content id", attachmentTestPart("image/png", "photo.png", "", "inline"), true},
		{"named application part", attachmentTestPart("application/octet-stream", "archive.zip", "", ""), true},
		{"unnamed application part", attachmentTestPart("application/octet-stream", "", "", ""), false},
		{"named text part", attachmentTestPart("text/plain", "notes.txt", "", ""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAttachmentPart(tt.part); got != tt.want {
				t.Errorf("isAttachmentPart() = %v, want %v", got, tt.want)
			}
			if got := hasAttachment(tt.part); got != tt.want {
				t.Errorf("hasAttachment() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasAttachmentWalksMultipartWithOnlyInlineImages(t *testing.T) {
	inline := attachmentTestPart("image/png", "logo.png", "logo@cid", "")
	text := &imap.BodyStructureSinglePart{Type: "text", Subtype: "html"}
	bs := &imap.BodyStructureMultiPart{Subtype: "related", Children: []imap.BodyStructure{text, inline}}
	if hasAttachment(bs) {
		t.Error("multipart with only HTML and an inline Content-ID image was marked as having an attachment")
	}
}
