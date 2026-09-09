// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"context"
	"strings"
	"testing"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

func TestForwardAttachmentsFetchesOnlyValidatedSelectedFiles(t *testing.T) {
	raw := crlf(`From: a@example.com
To: b@example.com
Subject: report
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary=files

--files
Content-Type: text/plain

Please read the report.
--files
Content-Type: application/pdf; name=report.pdf
Content-Disposition: attachment; filename=report.pdf
Content-Transfer-Encoding: base64

cGRmLWJ5dGVz
--files
Content-Type: image/png; name=logo.png
Content-Disposition: inline; filename=logo.png
Content-ID: <logo@example.com>
Content-Transfer-Encoding: base64

bG9nbw==
--files--
`)
	st := testStore(t)
	c := newTestIMAP(t, raw)
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	syncInbox(t, a, c)
	inbox, err := st.FolderBySpecial("acct", "inbox")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := st.MessageByFolderUID(inbox.ID, 1)
	if err != nil || msg == nil {
		t.Fatalf("synced message: %v", err)
	}
	drainCmds(t, a, c)
	selection := []store.ForwardAttachment{{Part: []int{2}, Filename: "forged.exe", ContentType: "application/forged"}}
	atts, problem := m.ForwardAttachments(context.Background(), "acct", msg.ID, selection, nil)
	if problem != "" || len(atts) != 1 || string(atts[0].Data) != "pdf-bytes" || atts[0].Filename != "report.pdf" || atts[0].ContentType != "application/pdf" {
		t.Fatalf("validated attachment: %+v; %s", atts, problem)
	}
	selection[0].Part = []int{3}
	if _, problem := m.ForwardAttachments(context.Background(), "acct", msg.ID, selection, nil); !strings.Contains(problem, "no longer available") {
		t.Fatalf("inline part was accepted: %s", problem)
	}
	selection[0].Part = []int{2}
	if _, problem := m.ForwardAttachments(context.Background(), "other", msg.ID, selection, nil); problem == "" {
		t.Fatal("cross-account source accepted")
	}
	if _, problem := m.ForwardAttachments(context.Background(), "acct", msg.ID, selection, []OutAttachment{{Data: make([]byte, MaxAttachTotal)}}); !strings.Contains(problem, "limit") {
		t.Fatalf("combined cap: %s", problem)
	}
}
