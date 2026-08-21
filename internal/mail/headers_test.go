// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"bytes"
	"context"
	"encoding/gob"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

// foldedRaw is the message these tests read: a folded header and two Received:
// lines, i.e. everything a parsed map would quietly destroy.
var foldedRaw = crlf(`Received: from a.example.com (a.example.com [10.0.0.1])
	by mx1.example.com with ESMTPS id aaa111
Received: from b.example.com (b.example.com [10.0.0.2])
	by mx2.example.com with ESMTPS id bbb222
From: <ada@example.com>
To: me@example.com
Subject: hi
Date: Mon, 02 Jan 2006 15:04:05 +0000
Message-ID: <hi@example.com>
X-Folded: one
 two
	three

hello
`)

// TestHeadersKeepFoldingAndDuplicates: the block is stored verbatim, because a
// parsed map cannot be turned back into one — folding and repeated fields are
// gone the moment it is parsed.
func TestHeadersKeepFoldingAndDuplicates(t *testing.T) {
	b := parseBody([]byte(foldedRaw))
	want := foldedRaw[:strings.Index(foldedRaw, "\r\n\r\n")+4]
	if b.headers != want {
		t.Errorf("header block not byte-preserved:\n got %q\nwant %q", b.headers, want)
	}
	h := parseHeaders(b.headers)
	if len(h["Received"]) != 2 {
		t.Errorf("Received survived as %q, want both lines", h["Received"])
	}
	if got := h["X-Folded"]; len(got) != 1 || got[0] != "one two three" {
		t.Errorf("folded header parsed as %q, want the continuation lines joined", got)
	}
}

// TestOldBlobDecodes: gob tolerates the added field, so a body cached before
// headers existed still decodes — no migration, no re-fetch storm.
func TestOldBlobDecodes(t *testing.T) {
	old := struct {
		HTML                string
		Text                string
		Inline              map[string]inlineDTO
		Calendar            []byte
		ListUnsubscribe     string
		ListUnsubscribePost string
	}{Text: "cached text"}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(old); err != nil {
		t.Fatal(err)
	}
	b, err := decodeBody(buf.Bytes())
	if err != nil {
		t.Fatalf("a pre-headers blob failed to decode: %v", err)
	}
	if b.textContent != "cached text" || b.headers != "" {
		t.Errorf("decoded %q / %q, want the body kept and headers empty", b.textContent, b.headers)
	}
}

// headerAccount syncs foldedRaw into a store-backed manager and leaves a
// goroutine draining a.cmds against c — the worker's drain, which is the only
// thing that ever runs a submitRO command.
func headerAccount(t *testing.T) (*Manager, *store.Message) {
	t.Helper()
	st := testStore(t)
	c := newTestIMAP(t, foldedRaw)
	m := NewManager(&config.Config{}, st)
	a := newTestAccount(m, "acct", "ok")
	syncInbox(t, a, c)

	inbox, err := st.FolderBySpecial("acct", "inbox")
	if err != nil || inbox == nil {
		t.Fatalf("FolderBySpecial(inbox) = %v, %v", inbox, err)
	}
	msg, err := st.MessageByFolderUID(inbox.ID, 1)
	if err != nil || msg == nil {
		t.Fatalf("synced message not found: %v", err)
	}
	drainCmds(t, a, c)
	return m, msg
}

// drainCmds runs queued commands against c until the test ends.
func drainCmds(t *testing.T, a *account, c *imapclient.Client) {
	t.Helper()
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case cm := <-a.cmds:
				cm.done <- cm.fn(c)
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() { close(stop) })
}

// TestHeadersPatchLegacyBlob: a body cached before headers were stored is
// patched by a header-only fetch. The cached text deliberately differs from
// what the server holds, so a full re-fetch would show up as it being replaced.
func TestHeadersPatchLegacyBlob(t *testing.T) {
	m, msg := headerAccount(t)
	legacy := parseBody([]byte(foldedRaw))
	legacy.headers = ""
	legacy.textContent = "cached text"
	blob, err := encodeBody(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.st.SaveMessageBody(msg.ID, blob); err != nil {
		t.Fatal(err)
	}

	raw, parsed, err := m.Headers(context.Background(), msg)
	if err != nil {
		t.Fatalf("Headers: %v", err)
	}
	if !strings.Contains(raw, "Subject: hi") || len(parsed["Received"]) != 2 {
		t.Errorf("header-only fetch returned %q", raw)
	}
	stored, ok, err := m.st.GetMessageBody(msg.ID)
	if err != nil || !ok {
		t.Fatalf("GetMessageBody = %v, %v", ok, err)
	}
	patched, err := decodeBody(stored)
	if err != nil {
		t.Fatal(err)
	}
	if patched.headers == "" {
		t.Error("the blob was not patched in place: the next read pays the fetch again")
	}
	if patched.textContent != "cached text" {
		t.Errorf("body content became %q: that was a full re-fetch, not a header patch", patched.textContent)
	}
	if b, ok := m.bodies.get(msg.ID); !ok || b.headers == "" {
		t.Error("the LRU entry still has no headers")
	}
}

// TestHeadersNoBlobFullFetch: with nothing cached, the full fetch is what
// resolves — and it caches a real body, not a headers-only stub that a later
// body read would mistake for an empty message.
func TestHeadersNoBlobFullFetch(t *testing.T) {
	m, msg := headerAccount(t)
	raw, parsed, err := m.Headers(context.Background(), msg)
	if err != nil {
		t.Fatalf("Headers: %v", err)
	}
	if !strings.Contains(raw, "Subject: hi") || len(parsed["From"]) == 0 {
		t.Errorf("headers not captured by the full fetch: %q", raw)
	}
	stored, ok, err := m.st.GetMessageBody(msg.ID)
	if err != nil || !ok {
		t.Fatalf("GetMessageBody = %v, %v", ok, err)
	}
	b, err := decodeBody(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.textContent, "hello") {
		t.Errorf("cached body is %q: a headers-only save would poison the body cache", b.textContent)
	}
}
