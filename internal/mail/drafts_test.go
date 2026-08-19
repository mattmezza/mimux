// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/store"
)

// draftAccount wires a store-backed manager and an account for c, with the
// server's folders already discovered — the state a push starts from once an
// account has synced at least once.
func draftAccount(t *testing.T, st *store.Store, c *imapclient.Client) *account {
	t.Helper()
	a := newTestAccount(NewManager(&config.Config{}, st), "acct", "syncing")
	a.cfg.Email = "me@example.com"
	if _, err := a.syncFolders(c); err != nil {
		t.Fatal(err)
	}
	return a
}

// draftCopy is one message as these tests care about it. Revisions of the same
// draft share a Message-ID, so they are counted, never keyed by it.
type draftCopy struct {
	MessageID string
	Deleted   bool
}

// draftCopies reports what the mailbox actually holds, in UID order.
func draftCopies(t *testing.T, c *imapclient.Client, folder string) []draftCopy {
	t.Helper()
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		t.Fatalf("select %s: %v", folder, err)
	}
	data, err := c.Fetch(imap.UIDSet{{Start: 1, Stop: 0}},
		&imap.FetchOptions{Flags: true, Envelope: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]draftCopy, 0, len(data))
	for _, msg := range data {
		id := ""
		if msg.Envelope != nil {
			id = strings.Trim(msg.Envelope.MessageID, "<>")
		}
		out = append(out, draftCopy{id, hasFlag(msg.Flags, imap.FlagDeleted)})
	}
	return out
}

// saveAndPush is the whole write-through cycle: local write first, then the
// IMAP publish, then re-read the row the way the next save would.
func saveAndPush(t *testing.T, a *account, c *imapclient.Client, d *store.Draft) *store.Draft {
	t.Helper()
	if err := a.m.st.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}
	if err := a.m.pushDraft(context.Background(), c, d); err != nil {
		t.Fatal(err)
	}
	got, err := a.m.st.DraftByID(d.ID)
	if err != nil || got == nil {
		t.Fatalf("DraftByID = %v, %v", got, err)
	}
	return got
}

// TestPushDraftReplacesTheRevision: saving a draft twice must leave one draft
// in the mailbox, not two — same Message-ID, new body, old copy expunged.
func TestPushDraftReplacesTheRevision(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t)
	if err := user.Create("Drafts", nil); err != nil {
		t.Fatal(err)
	}
	a := draftAccount(t, st, c)

	d := &store.Draft{Account: "acct", To: "ada@example.com", Subject: "Hi", Body: "one", Kind: "new"}
	first := saveAndPush(t, a, c, d)
	if first.MessageID == "" || first.UID == 0 || first.IMAPDirty {
		t.Fatalf("after the first push = %+v, want a recorded location and no debt", first)
	}
	drafts, _ := st.FolderBySpecial("acct", "drafts")
	if drafts == nil || first.FolderID != drafts.ID {
		t.Fatalf("draft filed in folder %d, want the Drafts folder", first.FolderID)
	}

	first.Body = "two"
	second := saveAndPush(t, a, c, first)
	if second.MessageID != first.MessageID {
		t.Errorf("Message-ID changed between revisions: %q then %q", first.MessageID, second.MessageID)
	}
	if second.UID == first.UID {
		t.Errorf("UID %d unchanged — the second revision was never appended", second.UID)
	}

	copies := draftCopies(t, c, "Drafts")
	if len(copies) != 1 {
		t.Fatalf("Drafts holds %d copies (%v), want exactly 1", len(copies), copies)
	}
	if copies[0].MessageID != second.MessageID || copies[0].Deleted {
		t.Errorf("the surviving copy is %+v, want the draft's own Message-ID, not deleted", copies[0])
	}
}

// draftRaw returns the raw bytes of the one surviving copy in a folder.
func draftRaw(t *testing.T, c *imapclient.Client, folder string) []byte {
	t.Helper()
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		t.Fatalf("select %s: %v", folder, err)
	}
	data, err := c.Fetch(imap.UIDSet{{Start: 1, Stop: 0}}, &imap.FetchOptions{
		Flags:       true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range data {
		if hasFlag(msg.Flags, imap.FlagDeleted) || len(msg.BodySection) == 0 {
			continue
		}
		return msg.BodySection[0].Bytes
	}
	t.Fatalf("%s holds no live copy", folder)
	return nil
}

// TestPushDraftCarriesAttachmentsAndBcc: what lands in the Drafts folder is the
// whole unfinished message — the files stored with it, and the Bcc line, which
// a send deliberately keeps out of the header and a draft must not lose.
func TestPushDraftCarriesAttachmentsAndBcc(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t)
	if err := user.Create("Drafts", nil); err != nil {
		t.Fatal(err)
	}
	a := draftAccount(t, st, c)

	d := &store.Draft{
		Account: "acct", To: "ada@example.com", Bcc: "quiet@example.com",
		Subject: "the numbers", Body: "see attached", Kind: "new",
	}
	if err := st.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDraftAttachment(d.ID, &store.DraftAttachment{
		Filename: "numbers.txt", ContentType: "text/plain", Data: []byte("41,42,43"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.m.pushDraft(context.Background(), c, d); err != nil {
		t.Fatal(err)
	}

	_, atts, err := parseDraftMessage(draftRaw(t, c, "Drafts"))
	if err != nil {
		t.Fatalf("the published draft does not parse back: %v", err)
	}
	if len(atts) != 1 || atts[0].Filename != "numbers.txt" || string(atts[0].Data) != "41,42,43" {
		t.Fatalf("attachments in the published copy = %+v, want the stored file", atts)
	}
	got, _, err := parseDraftMessage(draftRaw(t, c, "Drafts"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Bcc != "quiet@example.com" {
		t.Errorf("Bcc in the published copy = %q, want it kept — a draft is stored, not sent", got.Bcc)
	}
}

// foreignDraft leaves a draft in the Drafts folder the way another client
// would, syncs it in, and returns the stored message row for it.
func foreignDraft(t *testing.T, a *account, c *imapclient.Client, user *imapmemserver.User, in ComposeInput) *store.Message {
	t.Helper()
	raw, _, err := BuildMessage(config.Account{Name: "phone", Email: "me@example.com"}, in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := user.Append("Drafts", literal{bytes.NewReader(raw)},
		&imap.AppendOptions{Flags: []imap.Flag{imap.FlagDraft, imap.FlagSeen}}); err != nil {
		t.Fatal(err)
	}
	f, err := a.m.st.FolderBySpecial("acct", "drafts")
	if err != nil || f == nil {
		t.Fatalf("no Drafts folder: %v", err)
	}
	if _, err := a.syncFolder(context.Background(), c, f, c.Caps()); err != nil {
		t.Fatal(err)
	}
	msgs, err := a.m.st.ListMessages(f.ID, 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("ListMessages = %v, %v, want the appended draft", msgs, err)
	}
	return &msgs[0]
}

// TestAdoptDraftFromAnotherClient: a draft written on the phone opens here for
// editing, files and Bcc and all, and the first save replaces that very copy
// rather than leaving a second one beside it.
func TestAdoptDraftFromAnotherClient(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t)
	if err := user.Create("Drafts", nil); err != nil {
		t.Fatal(err)
	}
	a := draftAccount(t, st, c)
	msg := foreignDraft(t, a, c, user, ComposeInput{
		To: []string{"Ada <ada@example.com>"}, Cc: []string{"bob@example.com"},
		Bcc: []string{"quiet@example.com"}, Subject: "from the phone",
		Body: "half a thought", KeepBcc: true, MessageID: "phone-1@example.com",
		Attachments: []OutAttachment{{Filename: "photo.jpg", ContentType: "image/jpeg", Data: []byte("jpegbytes")}},
	})

	d, err := a.m.adoptDraft(context.Background(), c, msg)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	// The display name comes back RFC-quoted; a bare address stays bare.
	if d.To != `"Ada" <ada@example.com>` || d.Cc != "bob@example.com" || d.Bcc != "quiet@example.com" {
		t.Errorf("recipients = %q / %q / %q", d.To, d.Cc, d.Bcc)
	}
	if d.Subject != "from the phone" || !strings.Contains(d.Body, "half a thought") {
		t.Errorf("adopted draft = %+v", d)
	}
	if d.MessageID != msg.MessageID || d.FolderID != msg.FolderID || d.UID != msg.UID {
		t.Fatalf("adopted row = %+v, want it seeded from the mailbox copy %+v", d, msg)
	}
	if d.IMAPDirty {
		t.Error("adoption owes a push, but nothing has been edited yet")
	}
	atts, err := st.DraftAttachments(d.ID)
	if err != nil || len(atts) != 1 || atts[0].Filename != "photo.jpg" || string(atts[0].Data) != "jpegbytes" {
		t.Fatalf("adopted attachments = %+v, %v", atts, err)
	}

	// Opening it a second time is the same draft, not a second one.
	again, err := a.m.adoptDraft(context.Background(), c, msg)
	if err != nil || again.ID != d.ID {
		t.Fatalf("second open = %+v, %v, want the row the first one created (%d)", again, err, d.ID)
	}
	if all, _ := st.ListDrafts(); len(all) != 1 {
		t.Fatalf("%d local drafts after adopting one twice", len(all))
	}

	// Editing and saving replaces the copy it came from.
	d.Body = "a whole thought"
	saved := saveAndPush(t, a, c, d)
	copies := draftCopies(t, c, "Drafts")
	if len(copies) != 1 {
		t.Fatalf("Drafts holds %d copies (%v) after the first save, want 1", len(copies), copies)
	}
	if copies[0].MessageID != saved.MessageID {
		t.Errorf("surviving copy is %+v, want the adopted draft's own", copies[0])
	}
	got, _, err := parseDraftMessage(draftRaw(t, c, "Drafts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Body, "a whole thought") || got.Bcc != "quiet@example.com" {
		t.Errorf("republished draft = %+v, want the edit and the Bcc kept", got)
	}
}

// TestParseDraftMessageMapsHTMLToTheEditor: a rich-text draft read back off the
// server comes home as editor markup — Mode "html" and the body fragment,
// without the wrapper document's stylesheet leaking in as text.
func TestParseDraftMessageMapsHTMLToTheEditor(t *testing.T) {
	raw, _, err := BuildMessage(config.Account{Name: "W", Email: "me@example.com"}, ComposeInput{
		To: []string{"ada@example.com"}, Subject: "rich", Mode: "html",
		Body: "<p>Hello <b>you</b></p>", KeepBcc: true,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d, atts, err := parseDraftMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.Mode != "html" {
		t.Errorf("Mode = %q, want html", d.Mode)
	}
	if !strings.Contains(d.Body, "<b>you</b>") {
		t.Errorf("Body = %q, want the editor fragment", d.Body)
	}
	if strings.Contains(d.Body, "mimux-body h1") || strings.Contains(d.Body, "<style") {
		t.Errorf("the wrapper stylesheet came back as body text: %q", d.Body)
	}
	if len(atts) != 0 {
		t.Errorf("attachments = %+v, want none", atts)
	}
	if d.To != "ada@example.com" {
		t.Errorf("To = %q", d.To)
	}
}

// TestParseDraftMessageRefusesWhatItCannotEdit: a draft mimux cannot reproduce
// must fail loudly so the caller leaves it read-only, never adopt a version of
// it that quietly drops half the message.
func TestParseDraftMessageRefusesWhatItCannotEdit(t *testing.T) {
	cases := map[string]string{
		"encrypted": "From: a@b\r\nTo: c@d\r\nSubject: secret\r\n" +
			"Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=x\r\n\r\n" +
			"--x\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n--x--\r\n",
		"inline image": "From: a@b\r\nTo: c@d\r\nSubject: hi\r\n" +
			"Content-Type: multipart/related; boundary=y\r\n\r\n" +
			"--y\r\nContent-Type: text/html\r\n\r\n<p>look <img src=\"cid:pic\"></p>\r\n" +
			"--y\r\nContent-Type: image/png\r\nContent-ID: <pic>\r\n\r\nnotapng\r\n--y--\r\n",
		"not a message": "this is not a mail message at all",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if d, _, err := parseDraftMessage([]byte(raw)); err == nil {
				t.Errorf("adopted an uneditable draft as %+v, want a refusal", d)
			}
		})
	}
}

// TestPushDraftMarksTheDraftSeen: a draft the user is writing must not inflate
// their unread count.
func TestPushDraftMarksTheDraftSeen(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t)
	if err := user.Create("Drafts", nil); err != nil {
		t.Fatal(err)
	}
	a := draftAccount(t, st, c)
	saveAndPush(t, a, c, &store.Draft{Account: "acct", Subject: "Hi", Body: "x", Kind: "new"})

	if _, err := c.Select("Drafts", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.Fetch(imap.UIDSet{{Start: 1, Stop: 0}}, &imap.FetchOptions{Flags: true}).Collect()
	if err != nil || len(data) != 1 {
		t.Fatalf("fetch = %d messages, %v", len(data), err)
	}
	if !hasFlag(data[0].Flags, imap.FlagDraft) {
		t.Error("the appended copy is not flagged \\Draft")
	}
	if !hasFlag(data[0].Flags, imap.FlagSeen) {
		t.Error("the appended copy is unread: it will show up in the unread badge")
	}
}

// TestPushDraftWithoutUIDPlus: no UIDEXPUNGE means the old revision can only be
// marked \Deleted — expunging outright would take another client's deletions
// with it. The new revision still lands, which is the part that matters.
func TestPushDraftWithoutUIDPlus(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPCaps(t, imap.CapSet{imap.CapIMAP4rev1: {}})
	if err := user.Create("Drafts", nil); err != nil {
		t.Fatal(err)
	}
	a := draftAccount(t, st, c)

	d := &store.Draft{Account: "acct", Subject: "Hi", Body: "one", Kind: "new"}
	first := saveAndPush(t, a, c, d)
	first.Body = "two"
	second := saveAndPush(t, a, c, first)

	if second.IMAPDirty {
		t.Error("the draft still owes a push after a successful append")
	}
	if second.UID == 0 {
		t.Error("no APPENDUID and no Message-ID fallback: the next revision cannot replace this one")
	}
	copies := draftCopies(t, c, "Drafts")
	if len(copies) != 2 {
		t.Fatalf("Drafts holds %d copies (%v), want 2 — the old one can only be flagged, not expunged", len(copies), copies)
	}
	if !copies[0].Deleted {
		t.Error("the superseded revision is not marked \\Deleted")
	}
	if copies[1].Deleted || copies[1].MessageID != second.MessageID {
		t.Errorf("the current revision is %+v, want the live draft", copies[1])
	}
}

// TestPushDraftCreatesTheFolder: an account whose server has no Drafts mailbox
// gets one, rather than its drafts staying quietly local forever.
func TestPushDraftCreatesTheFolder(t *testing.T) {
	st := testStore(t)
	c, _ := newTestIMAPUser(t) // INBOX, Archive, Sent — no Drafts
	a := draftAccount(t, st, c)
	if f, _ := st.FolderBySpecial("acct", "drafts"); f != nil {
		t.Fatalf("the harness already has a Drafts folder: %+v", f)
	}

	got := saveAndPush(t, a, c, &store.Draft{Account: "acct", Subject: "Hi", Body: "x", Kind: "new"})

	f, err := st.FolderBySpecial("acct", "drafts")
	if err != nil || f == nil {
		t.Fatalf("FolderBySpecial(drafts) after push = %v, %v — the CREATE never happened", f, err)
	}
	if got.FolderID != f.ID || got.IMAPDirty {
		t.Errorf("draft = %+v, want it published into the new folder", got)
	}
	if copies := draftCopies(t, c, f.Name); len(copies) != 1 || copies[0].MessageID != got.MessageID {
		t.Errorf("%q holds %v, want the one draft", f.Name, copies)
	}
}

// TestPushDraftKeepsTheLocalRowWhenIMAPFails is the invariant: the store is
// written first, so a server that will not take the draft costs a retry, never
// the draft.
func TestPushDraftKeepsTheLocalRowWhenIMAPFails(t *testing.T) {
	st := testStore(t)
	d := &store.Draft{Account: "nosuchaccount", Subject: "Hi", Body: "x", Kind: "new"}
	if err := st.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}
	m := NewManager(&config.Config{}, st)
	if err := m.PushDraft(context.Background(), d); err == nil {
		t.Fatal("pushing to an unknown account did not error")
	}
	got, err := st.DraftByID(d.ID)
	if err != nil || got == nil {
		t.Fatalf("the draft is gone after a failed push: %v, %v", got, err)
	}
	if !got.IMAPDirty {
		t.Error("the failed push cleared the retry marker")
	}
}

// TestSteadyPushesOwedDrafts: a draft saved while the server was unreachable —
// or written before drafts were an IMAP thing at all, which is every row
// migration 0230 touches — is published by the next sync cycle, with nobody
// reopening compose.
func TestSteadyPushesOwedDrafts(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t)
	if err := user.Create("Drafts", nil); err != nil {
		t.Fatal(err)
	}
	// Saved with no connection in sight: local row, push owed.
	d := &store.Draft{Account: "acct", To: "ada@example.com", Subject: "owed", Body: "x", Kind: "new"}
	if err := st.UpsertDraft(d); err != nil {
		t.Fatal(err)
	}

	a := newTestAccount(NewManager(&config.Config{}, st), "acct", "syncing")
	a.cfg.Email = "me@example.com"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.session(ctx, c) }()

	eventually(t, "the owed draft to be published", func() bool {
		got, err := st.DraftByID(d.ID)
		return err == nil && got != nil && !got.IMAPDirty && got.UID != 0
	})

	cancel()
	<-done
}

// TestDropDraftRemovesAForeignCopy: deleting a draft written in another client
// takes the copy out of the mailbox. The row it deletes from is a draft that
// carries nothing but the copy's location — which is all the drafts page has
// for one it never adopted.
func TestDropDraftRemovesAForeignCopy(t *testing.T) {
	st := testStore(t)
	c, user := newTestIMAPUser(t)
	if err := user.Create("Drafts", nil); err != nil {
		t.Fatal(err)
	}
	a := draftAccount(t, st, c)
	msg := foreignDraft(t, a, c, user, ComposeInput{
		To: []string{"ada@example.com"}, Subject: "written elsewhere",
		Body: "delete me", MessageID: "phone-2@example.com",
	})

	if err := a.m.dropDraft(context.Background(), c,
		&store.Draft{Account: msg.Account, FolderID: msg.FolderID, UID: msg.UID}); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if copies := draftCopies(t, c, "Drafts"); len(copies) != 0 {
		t.Errorf("Drafts still holds %v after the delete", copies)
	}
	// UIDPLUS: the copy is really gone, so the local row goes with it.
	if left, _ := st.ListMessages(msg.FolderID, 10); len(left) != 0 {
		t.Errorf("%d mailbox row(s) left locally after the copy was expunged", len(left))
	}
}
