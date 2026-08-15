package store

import (
	"reflect"
	"testing"

	"github.com/mattmezza/sm/internal/config"
)

func TestAccountsCRUD(t *testing.T) {
	s := open(t)
	if a, _ := s.ListAccounts(); len(a) != 0 {
		t.Fatalf("fresh db has %d accounts", len(a))
	}

	acc := config.Account{
		Name: "work", SenderName: "Me", Provider: "gmail", Email: "me@gmail.com",
		Auth: "oauth2", OAuth2ClientID: "cid", OAuth2ClientSecret: "csecret",
		Aliases: []config.Alias{{Name: "Sales", Email: "sales@x.com"}},
	}
	if err := s.UpsertAccount(acc); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount("work")
	if err != nil || got == nil {
		t.Fatalf("GetAccount = %v, %v", got, err)
	}
	// Preset hosts filled on read.
	if got.IMAPHost != "imap.gmail.com" || got.SMTPHost != "smtp.gmail.com" {
		t.Errorf("preset not applied: %+v", got)
	}
	if len(got.Aliases) != 1 || got.Aliases[0].Email != "sales@x.com" {
		t.Errorf("aliases not round-tripped: %+v", got.Aliases)
	}
	if got.OAuth2ClientSecret != "csecret" {
		t.Errorf("secret not stored")
	}

	// Update: change sender name, keep the row.
	acc.SenderName = "Updated"
	if err := s.UpsertAccount(acc); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAccount("work")
	if got.SenderName != "Updated" {
		t.Errorf("update not applied: %+v", got)
	}
	if list, _ := s.ListAccounts(); len(list) != 1 {
		t.Errorf("upsert created a duplicate: %d rows", len(list))
	}

	// Delete removes it (and would cascade its mail).
	if err := s.DeleteAccount("work"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetAccount("work"); got != nil {
		t.Error("account still present after delete")
	}
}

func TestDeleteAccountCascadesMail(t *testing.T) {
	s := open(t)
	if err := s.UpsertAccount(config.Account{Name: "acc", Email: "a@b.c", IMAPHost: "i", SMTPHost: "s"}); err != nil {
		t.Fatal(err)
	}
	fid, err := s.UpsertFolder("acc", "INBOX", "inbox", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMessage(&Message{Account: "acc", FolderID: fid, UID: 1, Subject: "hi"}); err != nil {
		t.Fatal(err)
	}
	_ = s.setSetting(accountColorPrefix+"acc", "#fff")
	if err := s.DeleteAccount("acc"); err != nil {
		t.Fatal(err)
	}
	var folders, msgs int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM folders WHERE account='acc'`).Scan(&folders)
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE account='acc'`).Scan(&msgs)
	if folders != 0 || msgs != 0 {
		t.Errorf("mail not cascaded: %d folders, %d messages", folders, msgs)
	}
	if _, ok := s.getSetting(accountColorPrefix + "acc"); ok {
		t.Error("account color not removed")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	s := open(t)
	if err := s.UpsertAccount(config.Account{
		Name: "work", Provider: "gmail", Email: "me@gmail.com", Auth: "password", Password: "pw",
		Aliases: []config.Alias{{Name: "S", Email: "s@x.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAppConfig(AppConfig{TranslateAPIKey: "tk", TranslateTarget: "es", AIKey: "ak", AIModel: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken("work", StoredToken{Access: "a", Refresh: "r"}); err != nil {
		t.Fatal(err)
	}
	p := Prefs{MarkReadDelay: 2, SyncIntervalMin: 7, MaxPerSync: 123, AccountColors: map[string]string{"work": "#123456"}}
	if err := s.SavePrefs(p); err != nil {
		t.Fatal(err)
	}

	exp, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	if exp.Version != ExportVersion {
		t.Errorf("version = %d", exp.Version)
	}
	if len(exp.Accounts) != 1 || exp.Accounts[0].Password != "pw" {
		t.Errorf("account/credentials missing from export: %+v", exp.Accounts)
	}
	if exp.Settings["translate_api_key"] != "tk" || exp.Settings["ai_openrouter_key"] != "ak" {
		t.Errorf("keys missing from export")
	}
	if len(exp.Tokens) != 1 || exp.Tokens[0].Access != "a" {
		t.Errorf("tokens missing from export")
	}

	// Snapshot current state, import the same dump, verify no observable change.
	before, _ := s.ListAccounts()
	beforeCfg := s.GetAppConfig()
	beforePrefs := s.GetPrefs()

	sum, err := s.Import(exp)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Accounts != 1 {
		t.Errorf("import summary accounts = %d", sum.Accounts)
	}
	after, _ := s.ListAccounts()
	if !reflect.DeepEqual(before, after) {
		t.Errorf("accounts changed by round-trip:\n before %+v\n after %+v", before, after)
	}
	if !reflect.DeepEqual(beforeCfg, s.GetAppConfig()) {
		t.Errorf("app config changed by round-trip")
	}
	if !reflect.DeepEqual(beforePrefs, s.GetPrefs()) {
		t.Errorf("prefs changed by round-trip")
	}
}

func TestImportRejectsBadVersion(t *testing.T) {
	s := open(t)
	if _, err := s.Import(ConfigExport{Version: 999}); err == nil {
		t.Fatal("expected version error")
	}
}

func TestAccountSyncOverridesRoundTrip(t *testing.T) {
	s := open(t)
	if err := s.UpsertAccount(config.Account{Name: "acc", Email: "a@b.c", IMAPHost: "i", SMTPHost: "s"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetAccount("acc")
	if got.SyncIntervalMin != nil || got.MaxPerSync != nil || got.SyncMonths != nil || got.BodyCache != nil {
		t.Fatalf("expected no overrides by default: %+v", got)
	}

	interval, maxPerSync, months, bodyCache := 10, 50, 0, 25
	if err := s.SetAccountSyncOverrides("acc", &interval, &maxPerSync, &months, &bodyCache); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAccount("acc")
	if got.SyncIntervalMin == nil || *got.SyncIntervalMin != 10 ||
		got.MaxPerSync == nil || *got.MaxPerSync != 50 ||
		got.SyncMonths == nil || *got.SyncMonths != 0 ||
		got.BodyCache == nil || *got.BodyCache != 25 {
		t.Fatalf("overrides not round-tripped: %+v", got)
	}

	// A plain account-form save (UpsertAccount with no override fields set)
	// must not clobber the overrides set above.
	if err := s.UpsertAccount(config.Account{Name: "acc", Email: "a@b.c", IMAPHost: "i", SMTPHost: "s", SenderName: "Changed"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAccount("acc")
	if got.SyncIntervalMin == nil || *got.SyncIntervalMin != 10 ||
		got.BodyCache == nil || *got.BodyCache != 25 {
		t.Fatalf("account-form save clobbered the sync override: %+v", got)
	}

	// Clearing back to nil ("inherit global").
	if err := s.SetAccountSyncOverrides("acc", nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAccount("acc")
	if got.SyncIntervalMin != nil || got.MaxPerSync != nil || got.SyncMonths != nil || got.BodyCache != nil {
		t.Fatalf("overrides not cleared: %+v", got)
	}
}

func TestEffectiveSyncSettings(t *testing.T) {
	p := Prefs{SyncIntervalMin: 5, MaxPerSync: 500, SyncMonths: 6, BodyCache: 200}

	// No overrides: inherits every global value.
	interval, maxPerSync, months, bodyCache := EffectiveSyncSettings(p, config.Account{})
	if interval != 5 || maxPerSync != 500 || months != 6 || bodyCache != 200 {
		t.Fatalf("no override: got %d/%d/%d/%d", interval, maxPerSync, months, bodyCache)
	}

	// Partial override: only the set knobs change.
	n30, n50 := 30, 50
	interval, maxPerSync, months, bodyCache = EffectiveSyncSettings(p, config.Account{SyncIntervalMin: &n30, MaxPerSync: &n50})
	if interval != 30 || maxPerSync != 50 || months != 6 || bodyCache != 200 {
		t.Fatalf("partial override: got %d/%d/%d/%d", interval, maxPerSync, months, bodyCache)
	}

	// An explicit 0 override must win over a nonzero global, distinct from an
	// unset (nil) override — for sync months ("everything") and for the body
	// cache ("off"), which read opposite ways from the same 0.
	zero := 0
	_, _, months, _ = EffectiveSyncSettings(p, config.Account{SyncMonths: &zero})
	if months != 0 {
		t.Fatalf("explicit zero override lost: got %d", months)
	}
	if _, _, _, bodyCache = EffectiveSyncSettings(p, config.Account{BodyCache: &zero}); bodyCache != 0 {
		t.Fatalf("explicit zero body-cache override lost: got %d", bodyCache)
	}
}

// TestAccountStats: counters must be grouped per account, not summed across
// them, and must survive an account with folders but no messages.
func TestAccountStats(t *testing.T) {
	s := open(t)
	a, _ := s.UpsertFolder("A", "INBOX", "inbox", 0)
	_, _ = s.UpsertFolder("A", "Sent", "sent", 1)
	b, _ := s.UpsertFolder("B", "INBOX", "inbox", 0)
	for _, m := range []*Message{
		{Account: "A", FolderID: a, UID: 1, Size: 100},
		{Account: "A", FolderID: a, UID: 2, Size: 250, IsRead: true},
		{Account: "B", FolderID: b, UID: 1, Size: 7},
	} {
		if err := s.UpsertMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{a, b} {
		if err := s.RecountUnread(id); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.AccountStats()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]AccountStat{
		"A": {Messages: 2, Unread: 1, Folders: 2, Bytes: 350},
		"B": {Messages: 1, Unread: 1, Folders: 1, Bytes: 7},
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: got %+v want %+v", name, got[name], w)
		}
	}
}
