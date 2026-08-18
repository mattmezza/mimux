//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/ext"
	"github.com/mattmezza/mimux/internal/store"
)

// Licence enforcement covers this package and nothing else. Mail never stops:
// sync, send, search, the whole HTML client are AGPL code that has no idea this
// file exists. Without a valid licence or a live trial, exactly two things stop
// answering — /api/v1/* and /api/mcp — and they say so in a way that names the
// fix. /api/health and /api/v1/openapi.json stay open unconditionally: a probe
// and the documentation are not the product.
//
// Verification is offline and signature-only. There is no phone-home, no
// activation call, no machine fingerprint: a signed key is presented, ed25519
// says yes or no.

const (
	licencePrefix = "mimuxlic1"
	planAnnual    = "annual"
	planPerpetual = "perpetual"

	// graceDays is how long an expired annual licence keeps working. A renewal
	// key is emailed and pasted in by hand — mimux never phones home, so there
	// is no push channel — and someone who has already paid should not lose the
	// API to a missed email. Six-and-a-half weeks is long enough to survive a
	// holiday and a spam folder, and still short enough to mean something.
	graceDays = 45
	// trialDays is the no-key trial, from the first boot of a pro build.
	trialDays = 14
	// recheckAfter is how stale a cached evaluation may get. The key can change
	// under us (Settings → API writes the DB row), so the middleware re-reads,
	// but at most once a minute: this is a per-request path.
	recheckAfter = time.Minute

	accountURL = "https://account.mimux.dev"
)

// licencePubKeyB64 is the ed25519 public key licences are verified against,
// base64 (std alphabet, padded or raw) of the 32 raw bytes.
//
// The value committed here is a DEVELOPMENT key — its private half lives in
// licence_test.go as a test fixture and signs nothing anyone pays for.
// Production builds override it at link time:
//
//	go build -tags pro -ldflags "-X github.com/mattmezza/mimux/pro.licencePubKeyB64=<b64>" ./cmd/mimux
//
// A var rather than a const for exactly that reason.
var licencePubKeyB64 = "Mx7Z2AGAFR12g7cIkF+iQ5BzSwUlaWxSmWGKXMow6h4"

// licencePayload is the signed JSON body. The field set and names are the wire
// format shared with the issuer (account/licence.go) — the verifier never
// re-marshals, it verifies the exact bytes that arrived and then unmarshals
// them.
type licencePayload struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Plan      string `json:"plan"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt *int64 `json:"exp"` // nil for perpetual
	Watermark string `json:"watermark"`
}

// licenceState is one evaluation: what the middleware enforces, what it adds to
// the response, and one line of human summary for Settings and the CLI.
type licenceState struct {
	Allowed bool
	Code    string // 402 error code when !Allowed: licence_required | licence_version
	Message string // 402 message: what happened and what fixes it
	Warning string // X-Mimux-Licence-Warning, set during the grace period
	Trial   string // X-Mimux-Trial, set during the trial
	Line    string // one-line summary, stored for Settings and printed by the CLI
	Source  string // env | db | none
	Payload *licencePayload
}

// parseLicence verifies a key and returns its payload. The signature covers the
// decoded payload bytes, not their base64.
func parseLicence(key string) (licencePayload, error) {
	var p licencePayload
	parts := strings.Split(strings.TrimSpace(key), ".")
	if len(parts) != 3 || parts[0] != licencePrefix {
		return p, fmt.Errorf("not a %s key", licencePrefix)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return p, fmt.Errorf("payload is not base64url: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return p, fmt.Errorf("signature is not base64url: %w", err)
	}
	pub, err := licencePubKey()
	if err != nil {
		return p, err
	}
	if !ed25519.Verify(pub, payload, sig) {
		return p, fmt.Errorf("signature does not verify")
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return p, fmt.Errorf("payload is not valid JSON: %w", err)
	}
	return p, nil
}

func licencePubKey() (ed25519.PublicKey, error) {
	s := strings.TrimSpace(licencePubKeyB64)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil {
		return nil, fmt.Errorf("built-in public key is not base64: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("built-in public key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// evaluateLicence resolves the current state. Read-only on purpose: the trial's
// start row is written once, by newLicenceGate, so this runs unchanged against
// the read-only store `mimux licence status` opens while the server is up.
// st may be nil (no database at all — CLI on a fresh box with an env key).
func evaluateLicence(cfg *config.Config, st *store.Store, now time.Time) licenceState {
	key, source := licenceKey(cfg, st)
	if key == "" {
		return trialState(st, now, source, "")
	}
	p, err := parseLicence(key)
	if err != nil {
		// Garbage in the env or the DB is treated as no key at all: the trial
		// rules apply and the status says why, rather than hard-failing an
		// install over a mangled paste.
		return trialState(st, now, source, err.Error())
	}
	return checkLicence(p, cfg.Version, source, now)
}

// licenceKey returns the configured key and where it came from. The environment
// wins: an operator setting MIMUX_LICENCE_KEY should not have to also clear a
// stale row out of the database.
func licenceKey(cfg *config.Config, st *store.Store) (key, source string) {
	if cfg != nil && cfg.LicenceKey != "" {
		return cfg.LicenceKey, "env"
	}
	if st != nil {
		if v, ok := st.Setting(store.SettingLicenceKey); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), "db"
		}
	}
	return "", "none"
}

// checkLicence applies the plan rules to a verified payload.
func checkLicence(p licencePayload, buildVersion, source string, now time.Time) licenceState {
	s := licenceState{Source: source, Payload: &p, Allowed: true}
	who := maskEmail(p.Email)

	if p.Plan == planPerpetual || p.ExpiresAt == nil {
		// A perpetual licence buys the version you bought it at, forever. A
		// newer build is not covered — and that is said out loud rather than
		// silently degraded, because a silent degrade is how you find out via a
		// broken automation at 3am.
		if p.Plan == planPerpetual && buildAfterWatermark(buildVersion, p.Watermark) {
			s.Allowed = false
			s.Code = "licence_version"
			s.Message = fmt.Sprintf("This perpetual licence covers mimux up to %s; this build is %s. "+
				"Renew at %s to cover %s, or run %s. Mail itself keeps working either way.",
				p.Watermark, buildVersion, accountURL, buildVersion, p.Watermark)
			s.Line = fmt.Sprintf("Perpetual licence covers up to %s, this build is %s — API and MCP are paused (%s, from %s).",
				p.Watermark, buildVersion, who, source)
			return s
		}
		s.Line = fmt.Sprintf("Perpetual licence, covers builds up to %s (%s, from %s).", p.Watermark, who, source)
		return s
	}

	exp := time.Unix(*p.ExpiresAt, 0).UTC()
	graceEnd := exp.AddDate(0, 0, graceDays)
	switch {
	case now.Before(exp):
		s.Line = fmt.Sprintf("Annual licence, expires %s (%s, from %s).", day(exp), who, source)
	case now.Before(graceEnd):
		s.Warning = fmt.Sprintf("licence expired %s; API stops %s", day(exp), day(graceEnd))
		s.Line = fmt.Sprintf("Annual licence expired %s — grace period, API and MCP stop %s (%s, from %s).",
			day(exp), day(graceEnd), who, source)
	default:
		s.Allowed = false
		s.Code = "licence_required"
		s.Message = fmt.Sprintf("This licence expired on %s and its %d-day grace period ended on %s, "+
			"so the API and MCP endpoints are paused. Renew at %s. Your mail keeps syncing and sending as normal.",
			day(exp), graceDays, day(graceEnd), accountURL)
		s.Line = fmt.Sprintf("Annual licence expired %s, grace ended %s — API and MCP are paused (%s, from %s).",
			day(exp), day(graceEnd), who, source)
	}
	return s
}

// trialState is the no-usable-key path: a 14-day trial from the first boot of a
// pro build. invalid, when non-empty, is why the configured key was ignored.
func trialState(st *store.Store, now time.Time, source, invalid string) licenceState {
	s := licenceState{Source: source, Allowed: true}
	start, ok := trialStart(st)
	if !ok {
		// The row is written on the first pro boot; not finding one means this
		// process has not booted yet (the CLI reading ahead of the server), so
		// the trial has its full length in front of it.
		start = now
	}
	end := start.AddDate(0, 0, trialDays)
	left := int(end.Sub(now) / (24 * time.Hour))
	if end.Sub(now)%(24*time.Hour) > 0 {
		left++ // part of a day is still a day left, to the person reading it
	}
	switch {
	case now.Before(end):
		s.Trial = fmt.Sprintf("%d days left", left)
		s.Line = fmt.Sprintf("Trial — %d days left, ends %s.", left, day(end))
	default:
		s.Allowed = false
		s.Code = "licence_required"
		s.Message = fmt.Sprintf("The %d-day trial of the mimux pro layer ended on %s, so the API and MCP "+
			"endpoints are paused. Get a licence at %s and paste it into Settings → API. "+
			"Your mail keeps syncing and sending as normal.", trialDays, day(end), accountURL)
		s.Line = fmt.Sprintf("Trial ended %s — API and MCP are paused.", day(end))
	}
	if invalid != "" {
		s.Line = "Licence key is not valid (" + invalid + "). " + s.Line
	}
	return s
}

// trialStart reads when the trial began. ponytail: one app_settings row, and
// deleting it starts a fresh trial. That is not an oversight — anyone able to
// edit the SQLite file can also patch the binary, and a self-hosted product
// that fights its own user's database loses that fight while making the honest
// majority's life worse.
func trialStart(st *store.Store) (time.Time, bool) {
	if st == nil {
		return time.Time{}, false
	}
	v, ok := st.Setting(store.SettingProTrialStart)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// ensureTrialStart stamps the first pro boot. Written whether or not a licence
// is configured, so removing a key later does not mint a second trial.
func ensureTrialStart(st *store.Store, now time.Time) {
	if _, ok := trialStart(st); ok {
		return
	}
	if err := st.SetSetting(store.SettingProTrialStart, now.UTC().Format(time.RFC3339)); err != nil {
		slog.Warn("licence: recording trial start", "err", err)
	}
}

// buildAfterWatermark reports whether the running build is newer than what a
// perpetual licence covers. Versions are the repo's vMAJOR.MINOR tags; anything
// unparsable — "dev", a git hash, a blank — counts as covered, because locking
// a developer out of a build they just compiled is a bug, not enforcement.
func buildAfterWatermark(build, watermark string) bool {
	bMaj, bMin, bOK := parseVersion(build)
	wMaj, wMin, wOK := parseVersion(watermark)
	if !bOK || !wOK {
		return false
	}
	return bMaj > wMaj || (bMaj == wMaj && bMin > wMin)
}

// parseVersion pulls MAJOR.MINOR out of "v0.19", "0.19", "v0.19-pro", "v0.19.2".
func parseVersion(s string) (major, minor int, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	head, rest, found := strings.Cut(s, ".")
	if !found {
		return 0, 0, false
	}
	if i := strings.Index(rest, "."); i >= 0 {
		rest = rest[:i]
	}
	major, err := strconv.Atoi(head)
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(rest)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// maskEmail keeps enough to recognise your own licence, not enough to be worth
// a screenshot: "alice@example.com" -> "a…e@example.com".
func maskEmail(addr string) string {
	local, domain, ok := strings.Cut(addr, "@")
	if !ok || local == "" {
		return addr
	}
	if len(local) <= 2 {
		return local[:1] + "…@" + domain
	}
	return local[:1] + "…" + local[len(local)-1:] + "@" + domain
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// licenceGate is the enforcement point: one cached evaluation, refreshed lazily.
type licenceGate struct {
	cfg   *config.Config
	store *store.Store

	mu      sync.Mutex
	state   licenceState
	checked time.Time
	warned  time.Time // last daily grace warning
	written string    // last summary written to the DB
}

// newLicenceGate evaluates once at startup: the boot log says what the licence
// situation is before anyone has to discover it from a 402.
func newLicenceGate(deps ext.Deps) *licenceGate {
	g := &licenceGate{cfg: deps.Cfg, store: deps.Store}
	now := time.Now()
	ensureTrialStart(g.store, now)
	s := g.current(now)
	if s.Allowed {
		slog.Info("licence", "status", s.Line)
	} else {
		slog.Warn("licence: the API and MCP endpoints are paused (mail is unaffected)",
			"code", s.Code, "status", s.Line, "get one", accountURL)
	}
	return g
}

// current returns the cached state, re-evaluating at most once a minute so a
// key pasted into Settings takes effect without a restart.
func (g *licenceGate) current(now time.Time) licenceState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.checked.IsZero() && now.Sub(g.checked) < recheckAfter {
		return g.state
	}
	g.state, g.checked = evaluateLicence(g.cfg, g.store, now), now
	if g.state.Line != g.written {
		g.written = g.state.Line
		if err := g.store.SetSetting(store.SettingLicenceStatus, g.state.Line); err != nil {
			slog.Warn("licence: storing status", "err", err)
		}
	}
	if g.state.Warning != "" && now.Sub(g.warned) >= 24*time.Hour {
		g.warned = now
		slog.Warn("licence: " + g.state.Warning)
	}
	return g.state
}

// require is the chi middleware. It runs BEFORE tokenAuth on the groups it
// guards: a 402 must not spend a request from the token's rate-limit budget,
// and an unlicensed install should not be doing argon2 work either.
func (g *licenceGate) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := g.current(time.Now())
		if !s.Allowed {
			licenceError(w, s.Code, s.Message)
			return
		}
		// Warnings ride on successful responses: the client that will break
		// when the grace period runs out is the one making these calls.
		if s.Warning != "" {
			w.Header().Set("X-Mimux-Licence-Warning", s.Warning)
		}
		if s.Trial != "" {
			w.Header().Set("X-Mimux-Trial", s.Trial)
		}
		next.ServeHTTP(w, r)
	})
}

// licenceError is the 402 envelope: the same {"error":{...}} shape as every
// other failure, plus where to go about it.
func licenceError(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
			"details": map[string]string{"url": accountURL},
		},
	})
}
