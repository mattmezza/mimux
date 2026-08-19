//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// `mimux mail login <url>` — get a token without ever typing one.
//
// Copying a token out of Settings → API works and still does. It is also the
// step where people paste a credential into a chat window, or into the wrong
// terminal, or into a shell history file. So: the CLI opens the browser at the
// running mimux, the human approves what it may do in a session they already
// have, and the token comes back over a loopback listener this process owns.
//
// The verifier never leaves this process until the exchange, which is what
// makes the loopback port safe to race for: whoever wins the /callback still
// cannot spend the code. See internal/server/cliauth.go for the other half.

const credsFile = "credentials.json"

// credentials is what lands in ~/.config/mimux/credentials.json. Keyed by base
// URL, because one person plausibly has a laptop instance and a server one and
// should not have to say which every time.
type credentials struct {
	Default   string               `json:"default,omitempty"`
	Instances map[string]credEntry `json:"instances"`
}

type credEntry struct {
	Token    string `json:"token"`
	Label    string `json:"label,omitempty"`
	Scopes   string `json:"scopes,omitempty"`
	Insecure bool   `json:"insecure,omitempty"` // this instance's TLS is not verified
}

func credsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mimux", credsFile), nil
}

// loadCreds reads the store. A missing file is an empty store, not an error:
// not having logged in yet is the normal state of a fresh install.
func loadCreds() (credentials, error) {
	c := credentials{Instances: map[string]credEntry{}}
	path, err := credsPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(path) // #nosec G304 -- path is derived from os.UserConfigDir
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s is not readable as JSON: %w", path, err)
	}
	if c.Instances == nil {
		c.Instances = map[string]credEntry{}
	}
	return c, nil
}

// saveCreds writes the store back, 0600 in a 0700 directory. It holds bearer
// tokens: the modes are the point, not decoration.
func saveCreds(c credentials) error {
	path, err := credsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// normalizeURL is the credential store's key: one spelling per instance, so
// `--url http://host:8083/` and `--url http://host:8083` are the same login.
func normalizeURL(s string) string { return strings.TrimSuffix(strings.TrimSpace(s), "/") }

// storedBase picks the instance to talk to when nothing said which: the `use`
// default, or the only one there is. Anything less obvious than that is a
// question for the user, not a guess.
func storedBase(c credentials) string {
	if c.Default != "" {
		return c.Default
	}
	if len(c.Instances) == 1 {
		for k := range c.Instances {
			return k
		}
	}
	return ""
}

// applyCreds fills in the token from the store for whichever instance the flags
// settled on, and reports an unverified-TLS instance once per invocation. Called
// from do(), the single point every request passes through, so --url still wins
// over everything even though it is parsed after the client is built.
func (c *cliClient) applyCreds() {
	if c.credsDone {
		return
	}
	c.credsDone = true
	store, err := loadCreds()
	if err != nil {
		return // a broken store must not stop a command that carries its own token
	}
	e, ok := store.Instances[normalizeURL(c.base)]
	if !ok {
		return
	}
	if c.token == "" {
		c.token = e.Token
	}
	if e.Insecure {
		_, _ = fmt.Fprintf(c.errw, "warning: TLS certificates are not verified for %s (logged in with --insecure)\n", normalizeURL(c.base))
		c.http = insecureClient(c.http)
	}
}

// insecureClient returns a copy of h that skips certificate verification. For
// the self-signed-cert-on-the-LAN case, and opt-in per instance — never a
// global default, and it says so on every command that uses it.
func insecureClient(h *http.Client) *http.Client {
	out := *h
	out.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // #nosec G402 -- explicit --insecure
	return &out
}

// --- login ---

func cliLogin(c *cliClient, args []string) error {
	fs := c.flags("login")
	noBrowser := fs.Bool("no-browser", false, "print the URL instead of opening it, and read the code from stdin")
	scopes := fs.String("scopes", "mail:read mail:modify accounts:read", "scopes to ask for (the browser is where they are agreed)")
	label := fs.String("label", "", "how the token is named in Settings → API")
	insecure := fs.Bool("insecure", false, "do not verify TLS certificates for this instance (remembered)")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usageError{"want at most one URL"}
	}
	if len(rest) == 1 {
		c.base = rest[0]
	}
	base := normalizeURL(c.base)
	if base == "" {
		return usageError{"which mimux? pass a URL, e.g. `mimux mail login https://mail.example.com`"}
	}
	if *insecure {
		c.http = insecureClient(c.http)
	}
	if err := probePro(c, base); err != nil {
		return err
	}

	verifier := randomB64()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomB64()

	// Bound before the URL is built: the port is in it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("couldn't open a local port for the browser to come back to: %w", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	if *label == "" {
		host, _ := os.Hostname()
		*label = strings.TrimSpace("cli @ " + host)
	}
	q := url.Values{
		"port": {strconv.Itoa(port)}, "state": {state}, "challenge": {challenge},
		"name": {*label}, "scopes": {*scopes},
	}
	if *noBrowser {
		q.Set("nb", "1")
	}
	authURL := base + "/cli/auth?" + q.Encode()

	// Printed either way. An opener that silently failed, a browser on another
	// machine, an SSH session — the URL on stdout is the fallback for all of it.
	opened := false
	if !*noBrowser {
		opened = browserOpen(authURL) == nil
	}
	if opened {
		c.printf("Opened your browser to approve this login. If nothing happened, go to:\n%s\n", authURL)
	} else {
		c.printf("Open this in a browser to approve the login:\n%s\n", authURL)
	}

	code := ""
	if *noBrowser || !opened {
		code, err = promptCode(c)
	} else {
		code, err = awaitCallback(c, ln, state)
	}
	if err != nil {
		return err
	}

	tok, err := exchangeCode(c, base, code, verifier)
	if err != nil {
		return err
	}

	store, err := loadCreds()
	if err != nil {
		return err
	}
	store.Instances[base] = credEntry{
		Token: tok.Token, Label: tok.Label,
		Scopes: strings.Join(tok.Scopes, " "), Insecure: *insecure,
	}
	if store.Default == "" && len(store.Instances) > 1 {
		store.Default = base // second instance: pin the one just used, or every later command guesses
	}
	if err := saveCreds(store); err != nil {
		return err
	}
	path, _ := credsPath()
	c.printf("Signed in to %s as %q (%s). Saved to %s\n", base, tok.Label, strings.Join(tok.Scopes, " "), path)
	return nil
}

// probePro checks there is a pro mimux on the other end before opening anyone's
// browser. A free build has no /api at all, so a 404 here is the whole answer.
func probePro(c *cliClient, base string) error {
	res, err := c.http.Get(base + "/api/health")
	if err != nil {
		return fmt.Errorf("couldn't reach %s: %w (is mimux running?)", base, err)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	if res.StatusCode == http.StatusNotFound {
		return errors.New(base + " is a free build: it has no API, no MCP server and nothing for `mimux mail` to talk to.\n" +
			"mimux pro adds all three — https://mimux.dev/pricing/, or ask us at support@mimux.dev")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("%s answered %s at /api/health", base, res.Status)
	}
	return nil
}

// awaitCallback serves the one request the browser makes after approval. The
// state must match before the code is worth anything: a mismatch means this is
// not the flow we started, so stop rather than exchange whatever turned up.
func awaitCallback(c *cliClient, ln net.Listener, state string) (string, error) {
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			if q.Get("state") != state {
				http.Error(w, "That approval belongs to a different login. Nothing was collected.", http.StatusBadRequest)
				done <- result{err: errors.New("the browser came back with a state we did not send — nothing was exchanged")}
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, "You're signed in — close this tab.")
			done <- result{code: q.Get("code")}
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	// Shutdown, not Close: the handler has already handed the code over by the
	// time we get here, and Close would cut the "close this tab" page off
	// mid-flush.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	c.printf("%s\n", "Waiting for the browser…")
	select {
	case r := <-done:
		return r.code, r.err
	case <-time.After(5 * time.Minute):
		return "", errors.New("gave up waiting for the browser after 5 minutes")
	}
}

// promptCode is the --no-browser path (and the fallback when no opener exists):
// the page shows a code, the human carries it back across.
func promptCode(c *cliClient) (string, error) {
	c.printf("%s", "Code: ")
	line, err := bufio.NewReader(c.in).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	code := strings.TrimSpace(line)
	if code == "" {
		return "", errors.New("no code given")
	}
	return code, nil
}

type exchangeResult struct {
	Token  string   `json:"token"`
	Label  string   `json:"label"`
	Scopes []string `json:"scopes"`
}

func exchangeCode(c *cliClient, base, code, verifier string) (exchangeResult, error) {
	var out exchangeResult
	body, err := json.Marshal(map[string]string{"code": code, "verifier": verifier})
	if err != nil {
		return out, err
	}
	res, err := c.http.Post(base+"/cli/auth/exchange", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return out, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return out, err
	}
	if res.StatusCode != http.StatusOK {
		return out, fmt.Errorf("the code was refused (%s): %s", res.Status, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	if out.Token == "" {
		return out, errors.New("the exchange returned no token")
	}
	return out, nil
}

// browserOpen is openBrowser, indirected so a test can be the browser.
var browserOpen = openBrowser

// openBrowser hands the URL to whatever this platform uses to open one. No
// dependency: it is three command names and an exec.
func openBrowser(u string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name = "rundll32"
		args = []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}
	return exec.Command(name, append(args, u)...).Start() // #nosec G204 -- name is a literal
}

func randomB64() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// --- logout / use ---

func cliLogout(c *cliClient, args []string) error {
	fs := c.flags("logout")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	store, err := loadCreds()
	if err != nil {
		return err
	}
	var base string
	switch {
	case len(rest) == 1:
		base = normalizeURL(rest[0])
	case len(store.Instances) == 1:
		base = storedBase(store)
	default:
		return usageError{"which one? " + instanceList(store)}
	}
	if _, ok := store.Instances[base]; !ok {
		return errors.New("not signed in to " + base)
	}
	delete(store.Instances, base)
	if store.Default == base {
		store.Default = ""
	}
	if err := saveCreds(store); err != nil {
		return err
	}
	c.printf("Signed out of %s. The token still exists — revoke it in Settings → API.\n", base)
	return nil
}

func cliUse(c *cliClient, args []string) error {
	fs := c.flags("use")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	store, err := loadCreds()
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		c.printf("%s\n", instanceList(store))
		return nil
	}
	base := normalizeURL(rest[0])
	if _, ok := store.Instances[base]; !ok {
		return errors.New("not signed in to " + base + " — run `mimux mail login " + base + "` first")
	}
	store.Default = base
	if err := saveCreds(store); err != nil {
		return err
	}
	c.printf("Commands now default to %s\n", base)
	return nil
}

// instanceList renders what is signed in, marking the default.
func instanceList(store credentials) string {
	if len(store.Instances) == 0 {
		return "no instances — run `mimux mail login <url>`"
	}
	var b strings.Builder
	b.WriteString("signed in to:")
	for u, e := range store.Instances {
		b.WriteString("\n  " + u)
		if e.Label != "" {
			b.WriteString(" (" + e.Label + ")")
		}
		if u == store.Default {
			b.WriteString(" [default]")
		}
	}
	return b.String()
}
