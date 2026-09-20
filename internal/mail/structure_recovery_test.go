// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/mattmezza/mimux/internal/config"
)

func scriptedIMAP(t *testing.T, handler func(string) string) *imapclient.Client {
	t.Helper()
	client, server := net.Pipe()
	go func() {
		defer func() { _ = server.Close() }()
		_, _ = fmt.Fprint(server, "* OK ready\r\n")
		rd := bufio.NewScanner(server)
		for rd.Scan() {
			line := rd.Text()
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			response := handler(line)
			if response != "" {
				_, _ = fmt.Fprint(server, response)
			}
			_, _ = fmt.Fprintf(server, "%s OK done\r\n", fields[0])
		}
	}()
	c := imapclient.New(client, nil)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestMalformedStructureKeepsMetadataAndLaterUIDs(t *testing.T) {
	st := testStore(t)
	fid, err := st.UpsertFolder("acct", "INBOX", "inbox", 0)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := st.FolderByID(fid)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	attempts := 0
	a := &account{cfg: config.Account{Name: "acct"}, m: NewManager(&config.Config{}, st)}
	a.connectFn = func() (*imapclient.Client, error) {
		return scriptedIMAP(t, func(line string) string {
			if strings.Contains(strings.ToUpper(line), "BODYSTRUCTURE") {
				attempts++
				uid := 0
				for _, n := range []int{1, 2, 3} {
					if strings.Contains(line, fmt.Sprintf("FETCH %d ", n)) {
						uid = n
					}
				}
				if uid == 2 {
					return "* 2 FETCH (UID 2 BODYSTRUCTURE ((NIL) \"mixed\"))\r\n"
				}
				return fmt.Sprintf("* %d FETCH (UID %d BODYSTRUCTURE (\"text\" \"plain\" NIL NIL NIL \"7bit\" 5 1))\r\n", uid, uid)
			}
			if strings.Contains(strings.ToUpper(line), "SELECT") {
				return "* 3 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n"
			}
			return ""
		}), nil
	}
	main := scriptedIMAP(t, func(line string) string {
		if strings.Contains(strings.ToUpper(line), "FETCH") {
			var b strings.Builder
			for i := 1; i <= 3; i++ {
				fmt.Fprintf(&b, "* %d FETCH (UID %d FLAGS () ENVELOPE (\"Sun, 20 Sep 2026 12:00:00 +0000\" \"message %d\" NIL NIL NIL NIL NIL NIL NIL \"<id-%d>\") INTERNALDATE \"20-Sep-2026 12:00:00 +0000\" RFC822.SIZE 10)\r\n", i, i, i, i)
			}
			return b.String()
		}
		return ""
	})
	for pass := 0; pass < 2; pass++ {
		n, err := a.fetchSet(context.Background(), main, folder, imap.UIDSet{{Start: 1, Stop: 3}}, false)
		if err != nil {
			t.Fatal(err)
		}
		if pass == 0 && n != 3 {
			t.Fatalf("new messages = %d", n)
		}
		if pass == 1 && n != 0 {
			t.Fatalf("duplicate messages = %d", n)
		}
	}
	for i := uint32(1); i <= 3; i++ {
		msg, err := st.MessageByFolderUID(fid, i)
		if err != nil || msg == nil {
			t.Fatalf("UID %d missing: %v", i, err)
		}
		if (i == 2) != msg.StructureUnknown {
			t.Fatalf("UID %d unknown=%v", i, msg.StructureUnknown)
		}
	}
	if !strings.Contains(logs.String(), "uid=2") {
		t.Fatalf("UID absent from log: %s", logs.String())
	}
	if attempts != 3 {
		t.Fatalf("structure attempts=%d; want 3 without retrying bad UID", attempts)
	}
	if st.StructureRetryDue(fid, 2, time.Now()) {
		t.Fatal("bad UID retried immediately")
	}
}

func TestCacheEnvelopesDoesNotRequestMalformedStructure(t *testing.T) {
	st := testStore(t)
	fid, err := st.UpsertFolder("acct", "INBOX", "inbox", 0)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := st.FolderByID(fid)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(&config.Config{}, st)
	a := &account{cfg: config.Account{Name: "acct"}, m: m, cmds: make(chan cmd, 1), nudge: make(chan struct{}, 1)}
	m.accounts["acct"] = a
	c := scriptedIMAP(t, func(line string) string {
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "SELECT") {
			return "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n"
		}
		if strings.Contains(upper, "BODYSTRUCTURE") {
			t.Error("search requested BODYSTRUCTURE")
		}
		if strings.Contains(upper, "FETCH") {
			return "* 1 FETCH (UID 2 FLAGS () ENVELOPE (\"Sun, 20 Sep 2026 12:00:00 +0000\" \"search hit\" NIL NIL NIL NIL NIL NIL NIL \"<id-2>\") INTERNALDATE \"20-Sep-2026 12:00:00 +0000\" RFC822.SIZE 10)\r\n"
		}
		return ""
	})
	go func() { cm := <-a.cmds; cm.done <- cm.fn(c) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ids, err := m.CacheEnvelopes(ctx, "acct", folder, []uint32{2})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("ids=%v", ids)
	}
	msg, err := st.MessageByFolderUID(fid, 2)
	if err != nil || msg == nil || !msg.StructureUnknown {
		t.Fatalf("search metadata: %+v, %v", msg, err)
	}
}
