// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The Preview button is rendered only for Kind != "other", so attachmentKind is
// the whole "is this previewable" decision: MIME first, extension as fallback.
func TestAttachmentKind(t *testing.T) {
	cases := []struct {
		media, name, want string
	}{
		{"image/png", "shot.png", "image"},
		{"image/jpeg", "photo.jpg", "image"},
		{"application/pdf", "invoice.pdf", "pdf"},
		{"text/plain", "notes.txt", "text"},
		{"application/zip", "bundle.zip", "other"},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "cv.docx", "other"},
		{"application/octet-stream", "bundle.zip", "other"},
		// Extension fallback: senders that label everything octet-stream.
		{"application/octet-stream", "invoice.pdf", "pdf"},
		{"application/octet-stream", "shot.PNG", "image"},
		{"application/octet-stream", "server.log", "text"},
		{"", "readme.md", "text"},
		{"", "setup.exe", "other"},
		// A declared type wins over a misleading extension.
		{"image/png", "weird.zip", "image"},
	}
	for _, c := range cases {
		if got := attachmentKind(c.media, c.name); got != c.want {
			t.Errorf("attachmentKind(%q, %q) = %q, want %q", c.media, c.name, got, c.want)
		}
	}
}

// ...and the strip actually drops the button for those, while still offering
// Download.
func TestAttachmentStripHidesPreviewForOther(t *testing.T) {
	s := serverWith(t, nil, nil)
	w := httptest.NewRecorder()
	s.renderPartial(w, "attachments", map[string]any{"MsgID": int64(1), "Attachments": []attachmentView{
		{Part: "2", Name: "invoice.pdf", Size: "1 KB", Kind: attachmentKind("application/octet-stream", "invoice.pdf")},
		{Part: "3", Name: "bundle.zip", Size: "2 KB", Kind: attachmentKind("application/zip", "bundle.zip")},
	}})
	body := w.Body.String()
	if n := strings.Count(body, ">Preview<"); n != 1 {
		t.Errorf("Preview buttons = %d, want 1\n%s", n, body)
	}
	if n := strings.Count(body, ">Download<"); n != 2 {
		t.Errorf("Download links = %d, want 2\n%s", n, body)
	}
}
