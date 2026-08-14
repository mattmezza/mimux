package store

import (
	"testing"
	"time"
)

// TestSuggestAddresses: ranking (sent recipients and recent mail outrank a
// high-volume newsletter), case-insensitive dedupe, the latest display name
// winning, and the user's own identities never being suggested.
func TestSuggestAddresses(t *testing.T) {
	s := open(t)
	inbox, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	sent, _ := s.UpsertFolder("A", "Sent", "sent", 1)
	spam, _ := s.UpsertFolder("A", "Spam", "spam", 2)

	now := time.Now().UTC()
	uid := uint32(0)
	put := func(folder int64, fromName, from, to string, age time.Duration) {
		uid++
		if err := s.UpsertMessage(&Message{Account: "A", FolderID: folder, UID: uid,
			FromName: fromName, FromAddress: from, ToAddresses: to, Date: now.Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	old := 3 * 365 * 24 * time.Hour
	fresh := 24 * time.Hour

	// A newsletter with far more volume than anyone else, but all of it stale.
	for i := 0; i < 30; i++ {
		put(inbox, "News Daily", "news@example.com", "me@example.com", old)
	}
	// Someone we actually write to: 3 sends, recent.
	for i := 0; i < 3; i++ {
		put(sent, "Me", "me@example.com", "Bob@example.com, cc@example.com", fresh)
	}
	// Mixed case + a stale then fresh display name for the same address.
	put(inbox, "Old Name", "CAROL@example.com", "me@example.com", old)
	put(inbox, "Carol Smith", "carol@example.com", "me@example.com", fresh)
	// Spam senders must not be suggested at all.
	put(spam, "Nigerian Prince", "prince@example.com", "me@example.com", fresh)

	got, err := s.SuggestAddresses("example", []string{"ME@example.com"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	byAddr := map[string]AddressSuggestion{}
	for _, g := range got {
		if _, dup := byAddr[g.Address]; dup {
			t.Errorf("duplicate suggestion for %s", g.Address)
		}
		byAddr[g.Address] = g
	}
	if _, ok := byAddr["me@example.com"]; ok {
		t.Error("own identity suggested")
	}
	if _, ok := byAddr["prince@example.com"]; ok {
		t.Error("spam-folder sender suggested")
	}
	if _, ok := byAddr["bob@example.com"]; !ok {
		t.Errorf("sent recipient missing from %+v", got)
	}
	if n := byAddr["carol@example.com"].Name; n != "Carol Smith" {
		t.Errorf("display name = %q, want the most recent one", n)
	}
	if got[0].Address == "news@example.com" {
		t.Errorf("stale newsletter outranked live correspondents: %+v", got)
	}
	if d := byAddr["carol@example.com"].Display(); d != "Carol Smith <carol@example.com>" {
		t.Errorf("Display() = %q", d)
	}
	if d := byAddr["cc@example.com"].Display(); d != "cc@example.com" {
		t.Errorf("Display() without a name = %q", d)
	}
	// A comma in the display name would split the address list on send.
	if d := (AddressSuggestion{Address: "r@x.com", Name: "Oliveto, Rocco"}).Display(); d != "r@x.com" {
		t.Errorf("Display() with a comma in the name = %q", d)
	}

	// A one-character fragment is refused rather than scanning the mailbox.
	if sug, _ := s.SuggestAddresses("e", nil, 10); sug != nil {
		t.Errorf("1-char query returned %d suggestions", len(sug))
	}
	// LIKE wildcards typed by the user are literal, not "match anything".
	if sug, _ := s.SuggestAddresses("ca%l", nil, 10); len(sug) != 0 {
		t.Errorf("wildcard leaked into LIKE: %+v", sug)
	}
	// The limit survives dropping our own identities from an over-fetch.
	lim, _ := s.SuggestAddresses("example", []string{"me@example.com"}, 2)
	if len(lim) != 2 {
		t.Errorf("limit = %d suggestions, want 2", len(lim))
	}
}
