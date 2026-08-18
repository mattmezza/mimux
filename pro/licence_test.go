//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/ext"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/store"
)

// devPrivKeyB64 is the private half of the DEVELOPMENT key whose public half is
// the default of licencePubKeyB64 in licence.go. It is a TEST FIXTURE and
// nothing else: it signs the licences in this file so the verifier can be
// exercised end to end. It is not, and never was, the production signing key —
// that one exists only as an env var on the issuing server, and production
// builds override licencePubKeyB64 at link time (see licence.go).
const devPrivKeyB64 = "5y5NHUvagEf9QNZAO6Fs7SSKmYPGuwvhuxEgliUynDkzHtnYAYAVHXaDtwiQX6JDkHNLBSVpbFKZYYpcyjDqHg"

// signLicence mints a key the way account/ does: marshal once, sign those exact
// bytes, base64url both halves.
func signLicence(t *testing.T, key ed25519.PrivateKey, p licencePayload) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return licencePrefix + "." +
		base64.RawURLEncoding.EncodeToString(b) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, b))
}

func devKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	b, err := base64.RawStdEncoding.DecodeString(devPrivKeyB64)
	if err != nil || len(b) != ed25519.PrivateKeySize {
		t.Fatalf("dev private key fixture is broken: %v", err)
	}
	return ed25519.PrivateKey(b)
}

// annualKey signs an annual licence expiring at exp.
func annualKey(t *testing.T, exp time.Time) string {
	t.Helper()
	e := exp.Unix()
	return signLicence(t, devKey(t), licencePayload{
		ID: "lic_test", Email: "alice@example.com", Plan: planAnnual,
		IssuedAt: exp.AddDate(-1, 0, 0).Unix(), ExpiresAt: &e, Watermark: "v0.19",
	})
}

// perpetualKey signs a perpetual licence watermarked at a version.
func perpetualKey(t *testing.T, watermark string) string {
	t.Helper()
	return signLicence(t, devKey(t), licencePayload{
		ID: "lic_perp", Email: "alice@example.com", Plan: planPerpetual,
		IssuedAt: time.Now().Add(-time.Hour).Unix(), Watermark: watermark,
	})
}

// forceRecheck drops the cached evaluation, standing in for the up-to-a-minute
// wait the middleware would otherwise take to notice a changed key.
func (g *licenceGate) forceRecheck() {
	g.mu.Lock()
	g.checked = time.Time{}
	g.mu.Unlock()
}

func withKey(key string) func(*store.Store) {
	return func(st *store.Store) {
		if err := st.SetSetting(store.SettingLicenceKey, key); err != nil {
			panic(err)
		}
	}
}

func trialStartedAt(t time.Time) func(*store.Store) {
	return func(st *store.Store) {
		if err := st.SetSetting(store.SettingProTrialStart, t.UTC().Format(time.RFC3339)); err != nil {
			panic(err)
		}
	}
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) (code, url string) {
	t.Helper()
	var out struct {
		Error struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Details map[string]string `json:"details"`
		} `json:"error"`
	}
	decodeBody(t, rec, &out)
	if out.Error.Message == "" {
		t.Errorf("error envelope has no message: %s", rec.Body.String())
	}
	return out.Error.Code, out.Error.Details["url"]
}

// --- verification ---

func TestLicenceValidAnnual(t *testing.T) {
	ta := newTestAPIWith(t, nil, withKey(annualKey(t, time.Now().AddDate(0, 6, 0))))
	rec := ta.req(t, http.MethodGet, "/v1/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("accounts = %d: %s", rec.Code, rec.Body.String())
	}
	if h := rec.Header().Get("X-Mimux-Licence-Warning"); h != "" {
		t.Errorf("a live licence warns: %q", h)
	}
	if h := rec.Header().Get("X-Mimux-Trial"); h != "" {
		t.Errorf("a licensed install claims to be on trial: %q", h)
	}
	if line, _ := ta.st.Setting(store.SettingLicenceStatus); !strings.HasPrefix(line, "Annual licence, expires") {
		t.Errorf("status line stored for Settings = %q", line)
	}
}

func TestLicenceGracePeriodStillServes(t *testing.T) {
	exp := time.Now().AddDate(0, 0, -3)
	ta := newTestAPIWith(t, nil, withKey(annualKey(t, exp)))
	rec := ta.req(t, http.MethodGet, "/v1/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("in grace = %d, want the API still answering: %s", rec.Code, rec.Body.String())
	}
	warn := rec.Header().Get("X-Mimux-Licence-Warning")
	if !strings.Contains(warn, "licence expired "+day(exp)) ||
		!strings.Contains(warn, "API stops "+day(exp.AddDate(0, 0, graceDays))) {
		t.Errorf("grace warning = %q", warn)
	}
}

func TestLicencePastGraceIs402(t *testing.T) {
	ta := newTestAPIWith(t, nil, withKey(annualKey(t, time.Now().AddDate(0, 0, -(graceDays+1)))))
	rec := ta.req(t, http.MethodGet, "/v1/accounts", nil)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("past grace = %d, want 402: %s", rec.Code, rec.Body.String())
	}
	code, url := errorCode(t, rec)
	if code != "licence_required" || url != accountURL {
		t.Errorf("envelope = %q / %q", code, url)
	}
}

func TestLicencePerpetualWatermark(t *testing.T) {
	// Covered: the build is exactly the watermark.
	ta := newTestAPIWith(t, &config.Config{Version: "v0.19"}, withKey(perpetualKey(t, "v0.19")))
	if rec := ta.req(t, http.MethodGet, "/v1/accounts", nil); rec.Code != http.StatusOK {
		t.Fatalf("perpetual at watermark = %d: %s", rec.Code, rec.Body.String())
	}

	// Not covered: a newer build than the licence was bought for. It says so
	// rather than quietly degrading.
	ta = newTestAPIWith(t, &config.Config{Version: "v0.20-pro"}, withKey(perpetualKey(t, "v0.19")))
	rec := ta.req(t, http.MethodGet, "/v1/accounts", nil)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("perpetual past watermark = %d, want 402: %s", rec.Code, rec.Body.String())
	}
	code, _ := errorCode(t, rec)
	if code != "licence_version" {
		t.Errorf("code = %q, want licence_version", code)
	}
	if !strings.Contains(rec.Body.String(), "v0.19") || !strings.Contains(rec.Body.String(), "v0.20") {
		t.Errorf("the message names neither version: %s", rec.Body.String())
	}

	// A dev build is never locked out of its own machine.
	ta = newTestAPIWith(t, &config.Config{Version: "dev"}, withKey(perpetualKey(t, "v0.19")))
	if rec := ta.req(t, http.MethodGet, "/v1/accounts", nil); rec.Code != http.StatusOK {
		t.Fatalf("dev build with a perpetual licence = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLicenceGarbageFallsBackToTrial(t *testing.T) {
	// Right shape, wrong signer: the key must not verify.
	_, other, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	e := time.Now().AddDate(1, 0, 0).Unix()
	forged := signLicence(t, other, licencePayload{ID: "lic_x", Email: "eve@example.com", Plan: planAnnual, ExpiresAt: &e})

	for name, key := range map[string]string{"forged": forged, "junk": "not-a-licence", "truncated": "mimuxlic1.abc"} {
		ta := newTestAPIWith(t, nil, withKey(key))
		rec := ta.req(t, http.MethodGet, "/v1/accounts", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s key = %d, want the trial rules to apply: %s", name, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Mimux-Trial") == "" {
			t.Errorf("%s key: no trial header", name)
		}
		if line, _ := ta.st.Setting(store.SettingLicenceStatus); !strings.HasPrefix(line, "Licence key is not valid") {
			t.Errorf("%s key: status = %q", name, line)
		}
	}
}

// --- trial ---

func TestTrialFreshThenExpired(t *testing.T) {
	ta := newTestAPIWith(t, nil, nil) // no key, no row: the gate stamps day 0
	rec := ta.req(t, http.MethodGet, "/v1/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh trial = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Mimux-Trial"); got != "14 days left" {
		t.Errorf("X-Mimux-Trial = %q", got)
	}
	if _, ok := ta.st.Setting(store.SettingProTrialStart); !ok {
		t.Error("the first pro boot did not record a trial start")
	}

	ta = newTestAPIWith(t, nil, trialStartedAt(time.Now().AddDate(0, 0, -(trialDays+1))))
	rec = ta.req(t, http.MethodGet, "/v1/accounts", nil)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("expired trial = %d, want 402: %s", rec.Code, rec.Body.String())
	}
	code, url := errorCode(t, rec)
	if code != "licence_required" || url != accountURL {
		t.Errorf("envelope = %q / %q", code, url)
	}
}

func TestTrialStartIsStampedOnce(t *testing.T) {
	st := openStore(t)
	first := time.Now().AddDate(0, 0, -5)
	ensureTrialStart(st, first)
	ensureTrialStart(st, time.Now())
	got, ok := trialStart(st)
	if !ok || got.Unix() != first.Unix() {
		t.Errorf("trial start = %v (%v), want the first stamp %v", got, ok, first)
	}
}

// --- key sources ---

func TestEnvKeyBeatsDatabase(t *testing.T) {
	cfg := &config.Config{LicenceKey: annualKey(t, time.Now().AddDate(0, 6, 0))}
	ta := newTestAPIWith(t, cfg, withKey("garbage-in-the-database"))
	rec := ta.req(t, http.MethodGet, "/v1/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("env key = %d: %s", rec.Code, rec.Body.String())
	}
	line, _ := ta.st.Setting(store.SettingLicenceStatus)
	if !strings.Contains(line, "from env") {
		t.Errorf("status line = %q, want the env source", line)
	}
}

// --- middleware placement ---

// TestUnlicensedDoesNotSpendRateLimit: the licence check runs before the token
// check, so refusing a request costs the caller nothing from their budget.
func TestUnlicensedDoesNotSpendRateLimit(t *testing.T) {
	st := openStore(t)
	trialStartedAt(time.Now().AddDate(0, 0, -30))(st)
	cfg := &config.Config{API: config.API{RateLimitPerMinute: 1}}
	g := newLicenceGate(ext.Deps{Mail: mail.NewManager(cfg, st), Store: st, Cfg: cfg})
	tokens := newTokenAuth(st, 1)
	r := chi.NewRouter()
	r.With(g.require, tokens.require).Get("/v1/probe", probe)
	secret := mintToken(t, st, &store.APIToken{Label: "rl", Scopes: "mail:read"})

	for i := range 3 {
		if rec := call(r, secret); rec.Code != http.StatusPaymentRequired {
			t.Fatalf("call %d = %d, want 402", i, rec.Code)
		}
	}
	// Licence it: the one request per minute must still be there to spend.
	withKey(annualKey(t, time.Now().AddDate(0, 6, 0)))(st)
	g.forceRecheck()
	if rec := call(r, secret); rec.Code != http.StatusOK {
		t.Fatalf("first licensed call = %d, want 200 — the 402s ate the budget: %s", rec.Code, rec.Body.String())
	}
	if rec := call(r, secret); rec.Code != http.StatusTooManyRequests {
		t.Errorf("second licensed call = %d, want 429", rec.Code)
	}
}

// TestUnlicensedKeepsHealthAndSpecOpen: a probe and the documentation are not
// the product, and mail is not behind any of this.
func TestUnlicensedKeepsHealthAndSpecOpen(t *testing.T) {
	ta := newTestAPIWith(t, nil, trialStartedAt(time.Now().AddDate(0, 0, -30)))
	for _, path := range []string{"/health", "/v1/openapi.json"} {
		rec := httptest.NewRecorder()
		ta.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d without a licence, want 200", path, rec.Code)
		}
	}
}

func TestUnlicensedMCPIs402(t *testing.T) {
	ta := newTestAPIWith(t, nil, trialStartedAt(time.Now().AddDate(0, 0, -30)))
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ta.secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	ta.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("mcp = %d, want 402: %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorCode(t, rec); code != "licence_required" {
		t.Errorf("mcp code = %q", code)
	}
}

// --- version parsing ---

func TestBuildAfterWatermark(t *testing.T) {
	cases := []struct {
		build, watermark string
		want             bool
	}{
		{"v0.20", "v0.19", true},
		{"v1.0", "v0.99", true},
		{"v0.19", "v0.19", false},
		{"v0.19.3", "v0.19", false},
		{"v0.18", "v0.19", false},
		{"v0.20-pro", "v0.19", true},
		{"dev", "v0.19", false}, // unversioned build: covered
		{"v0.20", "", false},    // watermark-less licence: covered
		{"", "v0.19", false},    // no build version: covered
		{"garbage", "v0.19", false},
	}
	for _, c := range cases {
		if got := buildAfterWatermark(c.build, c.watermark); got != c.want {
			t.Errorf("buildAfterWatermark(%q, %q) = %v, want %v", c.build, c.watermark, got, c.want)
		}
	}
}

func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{
		"alice@example.com": "a…e@example.com",
		"al@example.com":    "a…@example.com",
		"nodomain":          "nodomain",
	} {
		if got := maskEmail(in); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- `mimux licence status` ---

func TestLicenceReportExitCodes(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		cfg      *config.Config
		seed     func(*store.Store)
		wantCode int
		contains []string
	}{
		{
			name: "valid annual", cfg: &config.Config{Version: "v0.19"},
			seed: withKey(annualKey(t, now.AddDate(0, 6, 0))), wantCode: 0,
			contains: []string{"plan:      annual", "a…e@example.com", "api:       answering", "saved in Settings"},
		},
		{
			name: "in grace", cfg: &config.Config{Version: "v0.19"},
			seed: withKey(annualKey(t, now.AddDate(0, 0, -2))), wantCode: 0,
			contains: []string{"grace:     day 3 of 14", "API stops"},
		},
		{
			name: "past grace", cfg: &config.Config{Version: "v0.19"},
			seed: withKey(annualKey(t, now.AddDate(0, 0, -(graceDays+2)))), wantCode: 1,
			contains: []string{"api:       paused"},
		},
		{
			name: "perpetual past watermark", cfg: &config.Config{Version: "v0.20"},
			seed: withKey(perpetualKey(t, "v0.19")), wantCode: 1,
			contains: []string{"expires:   never", "watermark: v0.19 (DOES NOT cover this build)"},
		},
		{
			name: "env source", cfg: &config.Config{Version: "v0.19", LicenceKey: perpetualKey(t, "v0.19")},
			wantCode: 0, contains: []string{"MIMUX_LICENCE_KEY", "covers this build"},
		},
		{
			name: "trial running", cfg: &config.Config{Version: "dev"},
			seed: trialStartedAt(now.AddDate(0, 0, -2)), wantCode: 0,
			contains: []string{"Trial — 12 days left", "trial:     started", "unversioned build"},
		},
		{
			name: "trial over", cfg: &config.Config{Version: "v0.19"},
			seed: trialStartedAt(now.AddDate(0, 0, -30)), wantCode: 1,
			contains: []string{"api:       paused", "Trial ended"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := openStore(t)
			if c.seed != nil {
				c.seed(st)
			}
			var b strings.Builder
			if got := licenceReport(&b, c.cfg, st, now); got != c.wantCode {
				t.Errorf("exit code = %d, want %d\n%s", got, c.wantCode, b.String())
			}
			for _, want := range c.contains {
				if !strings.Contains(b.String(), want) {
					t.Errorf("report missing %q:\n%s", want, b.String())
				}
			}
		})
	}
}

// TestLicenceReportWithoutStore: `mimux licence status` on a box with no
// database yet still reports the env key rather than crashing.
func TestLicenceReportWithoutStore(t *testing.T) {
	cfg := &config.Config{Version: "v0.19", LicenceKey: annualKey(t, time.Now().AddDate(0, 6, 0))}
	if got := licenceReport(io.Discard, cfg, nil, time.Now()); got != 0 {
		t.Errorf("exit code = %d, want 0", got)
	}
	if got := licenceReport(io.Discard, &config.Config{}, nil, time.Now()); got != 0 {
		t.Errorf("no key and no store = %d, want 0 (a trial that has not started yet)", got)
	}
}
