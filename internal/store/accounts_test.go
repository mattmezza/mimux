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
