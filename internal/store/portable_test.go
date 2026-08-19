// SPDX-License-Identifier: AGPL-3.0-only
package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/filter"
)

// seedConfig fills a store with one of everything the portable dump claims to
// cover, and returns the folder the labelled message lives in.
func seedConfig(t *testing.T, s *Store) {
	t.Helper()
	acc := config.Account{
		Name: "work", SenderName: "Me", Provider: "gmail", Email: "me@gmail.com",
		Auth: "oauth2", OAuth2ClientID: "cid", OAuth2ClientSecret: "csecret",
		Aliases: []config.Alias{{Name: "Sales", Email: "sales@x.com"}},
	}
	if err := s.UpsertAccount(acc); err != nil {
		t.Fatal(err)
	}
	months := 12
	max := 500
	bodyCache := 50
	if err := s.SetAccountSyncOverrides("work", nil, &max, &months, &bodyCache); err != nil {
		t.Fatal(err)
	}
	if err := s.setSetting("ai_api_key", "sk-test"); err != nil {
		t.Fatal(err)
	}
	// Appearance (Settings → Appearance) rides the generic app_settings dump
	// rather than a section of its own. Saved through the real path so this
	// still fails if the accent/icon look ever moves out of app_settings.
	look := s.GetAppConfig()
	look.Accent, look.IconBG, look.IconAccent = "violet", "transparent", "#ff0088"
	look.IconLeaf, look.IconShape = "#00ddaa", "circle"
	if err := s.SaveAppConfig(look); err != nil {
		t.Fatal(err)
	}
	// Prefs (Settings → Reading) ride the same generic app_settings dump; flip a
	// default-off one so the round trip fails if it ever moved out.
	prefs := s.GetPrefs()
	prefs.ShowListLabels = true
	prefs.AvatarShape = "square"
	// Notification *preferences* travel (they're just settings). The VAPID
	// private key and the push subscriptions deliberately do NOT — they live in
	// their own tables precisely so this dump can't carry them; see migration
	// 0160 and the assertions after the round trip.
	prefs.NotifyScope = "all"
	prefs.NtfyURL = "https://ntfy.sh/mimux-test-topic"
	if err := s.SavePrefs(prefs); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken("work", StoredToken{Access: "at", Refresh: "rt",
		Expiry: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	rule := &filter.Rule{Account: "work", Name: "Newsletters", Enabled: true,
		Conditions: []filter.Condition{{Field: "from", Op: "contains", Value: "news@"}},
		Actions:    []filter.Action{{Type: "label", Arg: "News"}}}
	if err := s.CreateRule(rule); err != nil {
		t.Fatal(err)
	}
	sig := &Signature{Account: "work", Name: "Work sig", TextPlain: "Matt",
		HTML: "<b>Matt</b>", Markdown: "**Matt**", ApplyMode: "first"}
	if err := s.UpsertSignature(sig); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIdentitySignature("Me@Gmail.com", sig.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTemplate(&Template{Name: "Intro", Body: "Hi there,"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSearch("Unread bills", "is:unread bill"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSenderAllowsExternal("boss@x.com", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSenderAllowsExternal("spam@x.com", false); err != nil {
		t.Fatal(err)
	}
}

// seedLabelledMessage puts one labelled message in a store. Both sides of the
// round trip need it: labels re-attach to an existing row, they don't create one.
func seedLabelledMessage(t *testing.T, s *Store, labels string) {
	t.Helper()
	f, err := s.UpsertFolder("work", "INBOX", "inbox", 0)
	if err != nil {
		t.Fatal(err)
	}
	m := &Message{Account: "work", FolderID: f, UID: 1, MessageID: "root@x", Labels: labels,
		Subject: "hi", Date: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	if err := s.UpsertMessage(m); err != nil {
		t.Fatal(err)
	}
}

// TestConfigRoundTrip: export everything from one instance, import into a
// second, and check each section came back — including the per-account sync
// overrides, which UpsertAccount alone would drop.
func TestConfigRoundTrip(t *testing.T) {
	src := open(t)
	seedConfig(t, src)
	seedLabelledMessage(t, src, "Work Project_X")

	exp, err := src.Export()
	if err != nil {
		t.Fatal(err)
	}
	if exp.Version != ExportVersion {
		t.Errorf("version = %d, want %d", exp.Version, ExportVersion)
	}

	dst := open(t)
	// The message is already there (re-synced from IMAP) but unlabelled: that is
	// the state a restore lands in.
	seedLabelledMessage(t, dst, "")
	sum, err := dst.Import(exp)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Accounts != 1 || sum.Tokens != 1 || sum.Filters != 1 || sum.Signatures != 1 ||
		sum.Templates != 1 || sum.Searches != 1 || sum.Senders != 1 || sum.Labels != 1 {
		t.Errorf("summary = %+v", sum)
	}

	acc, err := dst.GetAccount("work")
	if err != nil || acc == nil {
		t.Fatalf("GetAccount = %v, %v", acc, err)
	}
	if acc.OAuth2ClientSecret != "csecret" || len(acc.Aliases) != 1 {
		t.Errorf("account not restored: %+v", acc)
	}
	if acc.SyncIntervalMin != nil || acc.MaxPerSync == nil || *acc.MaxPerSync != 500 ||
		acc.SyncMonths == nil || *acc.SyncMonths != 12 ||
		acc.BodyCache == nil || *acc.BodyCache != 50 {
		t.Errorf("sync overrides not restored: %v %v %v %v",
			acc.SyncIntervalMin, acc.MaxPerSync, acc.SyncMonths, acc.BodyCache)
	}
	if v, _ := dst.getSetting("ai_api_key"); v != "sk-test" {
		t.Errorf("setting not restored: %q", v)
	}
	if look := dst.GetAppConfig(); look.Accent != "violet" || look.IconBG != "transparent" ||
		look.IconAccent != "#ff0088" || look.IconLeaf != "#00ddaa" || look.IconShape != "circle" {
		t.Errorf("appearance not restored: %+v", look)
	}
	if !dst.GetPrefs().ShowListLabels {
		t.Error("ShowListLabels not restored")
	}
	if dst.GetPrefs().AvatarShape != "square" {
		t.Errorf("AvatarShape not restored: %q", dst.GetPrefs().AvatarShape)
	}
	if p := dst.GetPrefs(); p.NotifyScope != "all" || p.NtfyURL != "https://ntfy.sh/mimux-test-topic" {
		t.Errorf("notification prefs not restored: %q %q", p.NotifyScope, p.NtfyURL)
	}
	if tok, _ := dst.GetToken("work"); tok == nil || tok.Refresh != "rt" {
		t.Errorf("token not restored: %+v", tok)
	}
	rules, _ := dst.ListRules()
	if len(rules) != 1 || rules[0].Name != "Newsletters" || len(rules[0].Conditions) != 1 ||
		rules[0].Actions[0].Arg != "News" {
		t.Errorf("filters not restored: %+v", rules)
	}
	sigs, _ := dst.ListSignatures()
	if len(sigs) != 1 || sigs[0].HTML != "<b>Matt</b>" || sigs[0].ApplyMode != "first" {
		t.Errorf("signatures not restored: %+v", sigs)
	}
	if id, _ := dst.IdentitySignatureID("me@gmail.com"); id != sigs[0].ID {
		t.Errorf("identity link not restored: %d want %d", id, sigs[0].ID)
	}
	tpls, _ := dst.ListTemplates()
	if len(tpls) != 1 || tpls[0].Body != "Hi there," {
		t.Errorf("templates not restored: %+v", tpls)
	}
	saved, _ := dst.ListSavedSearches()
	if len(saved) != 1 || saved[0].Query != "is:unread bill" {
		t.Errorf("saved searches not restored: %+v", saved)
	}
	if ok, _ := dst.SenderAllowsExternal("boss@x.com"); !ok {
		t.Error("trusted sender not restored")
	}
	if ok, _ := dst.SenderAllowsExternal("spam@x.com"); ok {
		t.Error("untrusted sender should not have been exported")
	}
	labels, _ := dst.DistinctLabels()
	if len(labels) != 1 || labels[0] != "Work Project_X" {
		t.Errorf("labels not re-attached: %v", labels)
	}

	// Idempotent: a second import of the same dump duplicates nothing.
	if _, err := dst.Import(exp); err != nil {
		t.Fatal(err)
	}
	rules, _ = dst.ListRules()
	sigs, _ = dst.ListSignatures()
	tpls, _ = dst.ListTemplates()
	saved, _ = dst.ListSavedSearches()
	accs, _ := dst.ListAccounts()
	if len(rules) != 1 || len(sigs) != 1 || len(tpls) != 1 || len(saved) != 1 || len(accs) != 1 {
		t.Errorf("re-import duplicated rows: %d rules, %d sigs, %d tpls, %d saved, %d accounts",
			len(rules), len(sigs), len(tpls), len(saved), len(accs))
	}
}

// TestExportOmitsPushSecrets: the backup file is cleartext and gets copied
// around, so the VAPID private key — the one credential that lets its holder
// push arbitrary notifications to the user's devices — must never appear in it,
// nor must the device subscriptions. They live outside app_settings for exactly
// this reason (migration 0160); this test is what stops someone "tidying" them
// back into it.
func TestExportOmitsPushSecrets(t *testing.T) {
	s := open(t)
	if err := s.SaveVAPIDKeys("pub-key-xyz", "priv-key-xyz"); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePushSub(PushSub{Endpoint: "https://push.example/abc", P256dh: "p", Auth: "a"}); err != nil {
		t.Fatal(err)
	}
	e, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"priv-key-xyz", "pub-key-xyz", "https://push.example/abc"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("export leaked %q", secret)
		}
	}
}

// TestImportV1Dump: a dump written before filters/signatures/templates existed
// must still import, not be rejected for its version or crash on the missing
// sections.
func TestImportV1Dump(t *testing.T) {
	s := open(t)
	v1 := ConfigExport{
		Version: 1,
		Accounts: []config.Account{{Name: "old", Provider: "gmail", Email: "old@gmail.com",
			Auth: "password", Password: "pw"}},
		Settings: map[string]string{"theme": "dark"},
		Tokens:   []TokenExport{{Account: "old", Access: "at", Refresh: "rt"}},
	}
	sum, err := s.Import(v1)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Accounts != 1 || sum.Settings != 1 || sum.Tokens != 1 || sum.Filters != 0 {
		t.Errorf("summary = %+v", sum)
	}
	if a, _ := s.GetAccount("old"); a == nil || a.Password != "pw" {
		t.Errorf("v1 account not imported: %+v", a)
	}
	if _, err := s.Import(ConfigExport{Version: ExportVersion + 1}); err == nil {
		t.Error("a newer-than-supported dump should be rejected")
	}
	if _, err := s.Import(ConfigExport{}); err == nil {
		t.Error("a version-less file should be rejected")
	}
}

// TestRoundTripAPITokensAndWebhooks: a restored install keeps honouring the
// tokens already handed out and keeps posting to the same endpoints with the
// same signing secrets — and importing twice does not double either.
func TestRoundTripAPITokensAndWebhooks(t *testing.T) {
	src := open(t)
	expires := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	if _, err := src.ImportAPIToken("Home Assistant", "argon2-hash-xyz", "mail:read mail:send",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Time{}, expires, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// A revoked one too: a restore must not bring a killed credential back.
	if _, err := src.ImportAPIToken("Old script", "argon2-hash-dead", "mail:read",
		time.Time{}, time.Time{}, time.Time{}, time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	paused := &WebhookEndpoint{URL: "https://hooks.example/mimux", Secret: "hook-secret-xyz",
		Events: "message.received", Active: false}
	if err := src.CreateWebhookEndpoint(paused); err != nil {
		t.Fatal(err)
	}
	// Its delivery log stays behind — see WebhookExport.
	if err := src.EnqueueWebhookDelivery(&WebhookDelivery{EndpointID: paused.ID,
		EventType: "message.received", DeliveryID: "d1", Payload: `{"marker":"not-in-the-dump"}`}); err != nil {
		t.Fatal(err)
	}

	exp, err := src.Export()
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal(exp)
	if strings.Contains(string(blob), "not-in-the-dump") {
		t.Error("the delivery log rode along in the dump")
	}

	dst := open(t)
	sum, err := dst.Import(exp)
	if err != nil {
		t.Fatal(err)
	}
	if sum.APITokens != 2 || sum.Webhooks != 1 {
		t.Errorf("summary = %d api tokens, %d webhooks", sum.APITokens, sum.Webhooks)
	}
	toks, _ := dst.ListAPITokens()
	if len(toks) != 2 {
		t.Fatalf("tokens restored: %d", len(toks))
	}
	byHash := map[string]APIToken{}
	for _, tok := range toks {
		byHash[tok.Hash] = tok
	}
	live := byHash["argon2-hash-xyz"]
	if live.Label != "Home Assistant" || live.Scopes != "mail:read mail:send" || !live.Live() {
		t.Errorf("live token did not survive: %+v", live)
	}
	if !live.ExpiresAt.Equal(expires) {
		t.Errorf("expiry = %v, want %v", live.ExpiresAt, expires)
	}
	if byHash["argon2-hash-dead"].Live() {
		t.Error("a revoked token came back alive")
	}

	eps, _ := dst.ListWebhookEndpoints()
	if len(eps) != 1 {
		t.Fatalf("endpoints restored: %d", len(eps))
	}
	if eps[0].Secret != "hook-secret-xyz" || eps[0].Events != "message.received" || eps[0].Active {
		t.Errorf("endpoint did not survive as configured: %+v", eps[0])
	}
	if log, _ := dst.ListWebhookDeliveries(eps[0].ID, 10); len(log) != 0 {
		t.Errorf("delivery log was restored: %+v", log)
	}

	// Second import: same rows, no duplicates.
	if _, err := dst.Import(exp); err != nil {
		t.Fatal(err)
	}
	toks, _ = dst.ListAPITokens()
	eps, _ = dst.ListWebhookEndpoints()
	if len(toks) != 2 || len(eps) != 1 {
		t.Errorf("re-import duplicated rows: %d tokens, %d endpoints", len(toks), len(eps))
	}
}
