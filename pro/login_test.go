//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// loginBox is one login test: an isolated config dir, a stub mimux, and a
// client with no token — the state a fresh install is actually in.
type loginBox struct {
	srv          *httptest.Server
	out, errw    bytes.Buffer
	exchanges    int
	approveState string // what the fake browser sends back; empty = echo what it got
}

func newLoginBox(t *testing.T, stdin string) (*loginBox, func(args ...string) int) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // linux
	t.Setenv("HOME", dir)            // macOS, and a backstop
	// A developer's own exports would otherwise decide these tests' answers.
	t.Setenv("MIMUX_URL", "")
	t.Setenv("MIMUX_TOKEN", "")
	t.Setenv("MIMUX_API_TOKEN", "")

	b := &loginBox{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	// Stands in for the approval page plus the human clicking Approve: it
	// redirects straight back to the CLI's loopback listener.
	mux.HandleFunc("/cli/auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		state := q.Get("state")
		if b.approveState != "" {
			state = b.approveState
		}
		http.Redirect(w, r, "http://127.0.0.1:"+q.Get("port")+
			"/callback?state="+url.QueryEscape(state)+"&code=the-one-shot-code", http.StatusSeeOther)
	})
	mux.HandleFunc("/cli/auth/exchange", func(w http.ResponseWriter, r *http.Request) {
		b.exchanges++
		var req struct{ Code, Verifier string }
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Code != "the-one-shot-code" || req.Verifier == "" {
			http.Error(w, "bad code", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{
			"token": "mimux_pat_fromlogin", "label": "cli @ test",
			"scopes": []string{"mail:read", "mail:modify"},
		})
	})
	mux.HandleFunc("/api/v1/tokens/self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"label": r.Header.Get("Authorization"), "scopes": []string{"mail:read"}})
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)

	// The fake browser: open the URL and follow the redirect, which lands on
	// the CLI's own listener. Exactly what a real one does.
	t.Cleanup(func(prev func(string) error) func() {
		return func() { browserOpen = prev }
	}(browserOpen))
	browserOpen = func(u string) error {
		go func() {
			res, err := http.Get(u) // #nosec G107 -- test URL
			if err == nil {
				_ = res.Body.Close()
			}
		}()
		return nil
	}

	// newCLIClient, not a literal: resolving the base URL from the stored
	// default is half of what these tests are about.
	return b, func(args ...string) int {
		return newCLIClient(&b.out, &b.errw, strings.NewReader(stdin), b.srv.Client()).dispatch(args)
	}
}

func readCreds(t *testing.T) credentials {
	t.Helper()
	c, err := loadCreds()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The whole round trip: browser out, loopback back, code exchanged, token on
// disk — and the next command uses it without being told.
func TestLoginRoundTrip(t *testing.T) {
	b, run := newLoginBox(t, "")
	if code := run("login", b.srv.URL); code != 0 {
		t.Fatalf("exit %d: %s", code, b.errw.String())
	}
	if b.exchanges != 1 {
		t.Errorf("exchanges = %d, want 1", b.exchanges)
	}
	if !strings.Contains(b.out.String(), "Signed in to") {
		t.Errorf("stdout = %q", b.out.String())
	}

	store := readCreds(t)
	e, ok := store.Instances[b.srv.URL]
	if !ok || e.Token != "mimux_pat_fromlogin" || e.Label != "cli @ test" {
		t.Fatalf("stored = %+v", store)
	}

	path, _ := credsPath()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("credentials.json is %v, want 0600 — it holds a bearer token", fi.Mode().Perm())
	}
	if di, err := os.Stat(strings.TrimSuffix(path, "/"+credsFile)); err == nil && di.Mode().Perm() != 0o700 {
		t.Errorf("the config dir is %v, want 0700", di.Mode().Perm())
	}

	// A later command finds the token itself: the single stored instance is
	// also the resolved base URL.
	b.out.Reset()
	if code := run("whoami"); code != 0 {
		t.Fatalf("whoami exit %d: %s", code, b.errw.String())
	}
	if !strings.Contains(b.out.String(), "Bearer mimux_pat_fromlogin") {
		t.Errorf("whoami did not use the stored token:\n%s", b.out.String())
	}
}

// A callback whose state is not the one we sent is not our flow: stop, and do
// not hand the verifier over to whoever sent it.
func TestLoginStateMismatchAborts(t *testing.T) {
	b, run := newLoginBox(t, "")
	b.approveState = "someone-elses-state"
	if code := run("login", b.srv.URL); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, b.errw.String())
	}
	if b.exchanges != 0 {
		t.Errorf("exchanged despite a state mismatch (%d calls)", b.exchanges)
	}
	if !strings.Contains(b.errw.String(), "state") {
		t.Errorf("stderr = %q", b.errw.String())
	}
	if len(readCreds(t).Instances) != 0 {
		t.Error("a failed login still wrote credentials")
	}
}

// --no-browser: the URL goes to stdout with nb=1 and the code comes back on
// stdin. No listener, no opener.
func TestLoginNoBrowser(t *testing.T) {
	b, run := newLoginBox(t, "the-one-shot-code\n")
	browserOpen = func(string) error { t.Error("--no-browser still opened a browser"); return nil }
	if code := run("login", "-no-browser", b.srv.URL); code != 0 {
		t.Fatalf("exit %d: %s", code, b.errw.String())
	}
	out := b.out.String()
	if !strings.Contains(out, "nb=1") || !strings.Contains(out, "/cli/auth?") {
		t.Errorf("stdout does not carry the URL to open:\n%s", out)
	}
	if b.exchanges != 1 {
		t.Errorf("exchanges = %d, want 1", b.exchanges)
	}
	if readCreds(t).Instances[b.srv.URL].Token != "mimux_pat_fromlogin" {
		t.Error("--no-browser did not store the token")
	}
}

// A free build has no /api at all, so the CLI says so and never opens anyone's
// browser at it.
func TestLoginRefusesFreeBuild(t *testing.T) {
	b, run := newLoginBox(t, "")
	browserOpen = func(string) error { t.Error("opened a browser against a free build"); return nil }
	free := httptest.NewServer(http.NotFoundHandler())
	defer free.Close()
	if code := run("login", free.URL); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(b.errw.String(), "pricing") {
		t.Errorf("stderr = %q, want the conversion note", b.errw.String())
	}
}

// use picks the default when there is more than one instance; logout needs no
// argument when there is only one left.
func TestUseAndLogout(t *testing.T) {
	b, run := newLoginBox(t, "")
	if err := saveCreds(credentials{Instances: map[string]credEntry{
		"https://a.example": {Token: "t-a"},
		"https://b.example": {Token: "t-b"},
	}}); err != nil {
		t.Fatal(err)
	}

	if code := run("use", "https://b.example"); code != 0 {
		t.Fatalf("use exit %d: %s", code, b.errw.String())
	}
	if got := readCreds(t).Default; got != "https://b.example" {
		t.Errorf("default = %q", got)
	}
	// An instance nobody is signed in to is a mistake, not a new default.
	if code := run("use", "https://c.example"); code != 1 {
		t.Errorf("use on an unknown instance = %d, want 1", code)
	}

	// Two instances, no argument: ask rather than guess.
	if code := run("logout"); code != 2 {
		t.Errorf("bare logout with two instances = %d, want 2 (usage)", code)
	}
	if code := run("logout", "https://b.example/"); code != 0 { // trailing slash still matches
		t.Fatalf("logout exit %d: %s", code, b.errw.String())
	}
	store := readCreds(t)
	if _, ok := store.Instances["https://b.example"]; ok {
		t.Error("logout left the entry behind")
	}
	if store.Default != "" {
		t.Errorf("logout left a dangling default %q", store.Default)
	}
	// One left: no argument needed.
	if code := run("logout"); code != 0 {
		t.Fatalf("bare logout exit %d: %s", code, b.errw.String())
	}
	if len(readCreds(t).Instances) != 0 {
		t.Error("credentials survived the last logout")
	}
}

// An instance logged in with --insecure says so on every command that uses it,
// and the flag persists rather than being re-typed.
func TestInsecureIsRememberedAndWarned(t *testing.T) {
	b, run := newLoginBox(t, "")
	if code := run("login", "-insecure", b.srv.URL); code != 0 {
		t.Fatalf("exit %d: %s", code, b.errw.String())
	}
	if !readCreds(t).Instances[b.srv.URL].Insecure {
		t.Fatal("--insecure was not persisted")
	}
	b.errw.Reset()
	if code := run("whoami"); code != 0 {
		t.Fatalf("whoami exit %d: %s", code, b.errw.String())
	}
	if !strings.Contains(b.errw.String(), "not verified") {
		t.Errorf("no warning on a later command: %q", b.errw.String())
	}
}

// Without a stored token and without MIMUX_TOKEN, the CLI points at login
// rather than dumping a 401.
func TestNoCredentialsPointsAtLogin(t *testing.T) {
	b, run := newLoginBox(t, "")
	if code := run("accounts", "-url", b.srv.URL); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(b.errw.String(), "mimux mail login") {
		t.Errorf("stderr = %q", b.errw.String())
	}
}

// The credential file is the CLI's own format; a round trip through it must not
// quietly drop the default marker.
func TestCredentialsRoundTrip(t *testing.T) {
	_, _ = newLoginBox(t, "")
	in := credentials{Default: "https://a.example", Instances: map[string]credEntry{
		"https://a.example": {Token: "t", Label: "l", Scopes: "mail:read", Insecure: true},
	}}
	if err := saveCreds(in); err != nil {
		t.Fatal(err)
	}
	out, err := loadCreds()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(in)
	bb, _ := json.Marshal(out)
	if string(a) != string(bb) {
		t.Errorf("round trip: %s != %s", a, bb)
	}
}
