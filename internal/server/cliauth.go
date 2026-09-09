// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/store"
)

// Browser-based login for `mimux mail login <url>`.
//
// The CLI cannot show a password box and should not ask for one: it wants a
// scoped API token, and the only thing that may hand one out is a session
// already signed in here. So the CLI opens this page in the browser, the human
// approves what it may do, and the token travels back over a loopback redirect
// the CLI is itself listening on.
//
// The shape is OAuth's authorization-code-with-PKCE, minus the parts that exist
// for third-party clients:
//
//   - PKCE, because the redirect goes to 127.0.0.1 and any other local process
//     could race for the port. The code alone is worthless without the verifier
//     that only the CLI that started the flow holds.
//   - A nonce cookie set on the GET and re-checked on the POST (same trick as
//     the OAuth flow next door), so a tab left open from a login that was
//     abandoned an hour ago cannot mint a token when someone clicks Approve.
//   - Codes live in memory for two minutes and are deleted on read. No table
//     and no migration: a code that does not survive a restart is a code doing
//     its job, and the whole flow is a browser tab and a terminal apart.
//
// What it deliberately is not: a client registry, a refresh token, a consent
// record. There is one client, it is on this machine, and the token it gets is
// the same token Settings → API mints.

const (
	cliExchangePath = "/cli/auth/exchange"
	cliAuthNonce    = "mimux_cli_nonce"
	cliCodeTTL      = 2 * time.Minute
	cliDefaultLabel = "mimux CLI"
	cliLoopbackHost = "127.0.0.1" // a literal, never anything from the request
	cliMinPort      = 1024        // below this needs root: not a CLI's listener
)

// cliTicket is one approved-but-not-yet-collected login.
type cliTicket struct {
	secret    string // the plaintext API token, handed over exactly once
	label     string
	scopes    string
	challenge string // base64url(SHA256(verifier)) as the CLI stated it
	expiresAt time.Time
}

// putCLITicket stores a ticket and sweeps the expired ones on the way past.
// ponytail: lazy sweep under the same lock — the map holds at most the logins
// started in the last two minutes, which is a number a human types.
func (s *Server) putCLITicket(code string, t cliTicket) {
	s.cliMu.Lock()
	defer s.cliMu.Unlock()
	now := time.Now()
	for c, old := range s.cliTickets {
		if now.After(old.expiresAt) {
			delete(s.cliTickets, c)
		}
	}
	s.cliTickets[code] = t
}

// takeCLITicket returns a ticket and removes it, whatever happens next. A code
// is good for one attempt: a wrong verifier burns it too, so guessing at one
// costs a whole flow rather than a request.
func (s *Server) takeCLITicket(code string) (cliTicket, bool) {
	s.cliMu.Lock()
	defer s.cliMu.Unlock()
	t, ok := s.cliTickets[code]
	delete(s.cliTickets, code)
	return t, ok
}

// cliPort validates the loopback port the CLI is listening on. It ends up in a
// Location header, so it is parsed as a number and re-emitted as one — never
// echoed back as the string that arrived.
func cliPort(v string) (int, error) {
	p, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || p < cliMinPort || p > 65535 {
		return 0, errors.New("port must be a number between " + strconv.Itoa(cliMinPort) + " and 65535")
	}
	return p, nil
}

// cliScopes reads the scopes the CLI asked for, however it spelled them
// (repeated params, spaces or commas), and filters them through the same
// allow-list Settings → API uses. Nothing recognised means mail:read.
func cliScopes(values []string) []string {
	fields := strings.FieldsFunc(strings.Join(values, " "), func(r rune) bool {
		return r == ' ' || r == ',' || r == '+'
	})
	return strings.Fields(store.ValidScopes(fields))
}

// handleCLIAuthForm renders the approval page. It sits inside the auth group,
// so an unauthenticated CLI login walks through /login first and comes back
// here — that is what the `next` parameter is for.
func (s *Server) handleCLIAuthForm(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	port, err := cliPort(q.Get("port"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	state, challenge := q.Get("state"), q.Get("challenge")
	if state == "" || challenge == "" {
		http.Error(w, "this link is missing its state or challenge — start `mimux mail login` again", http.StatusBadRequest)
		return
	}
	nonce := auth.NewToken()
	http.SetCookie(w, &http.Cookie{
		Name: cliAuthNonce, Value: nonce, Path: "/cli/auth",
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	wanted := map[string]bool{}
	for _, sc := range cliScopes(q["scopes"]) {
		wanted[sc] = true
	}
	label := strings.TrimSpace(q.Get("name"))
	if label == "" {
		label = cliDefaultLabel
	}
	s.renderRequest(w, r, "cli_auth", map[string]any{
		"CSRF":      auth.EnsureCSRF(w, r, s.secure),
		"Nonce":     nonce,
		"Port":      port,
		"State":     state,
		"Challenge": challenge,
		"Label":     label,
		"NoBrowser": q.Get("nb") == "1",
		"Scopes":    store.APIScopes,
		"Wanted":    wanted,
		"Pro":       s.proView(),
	})
}

// handleCLIAuthApprove mints the token and hands the CLI a code to collect it
// with. The form's checkboxes are authoritative, not the query string that
// pre-ticked them: what the human saw and left ticked is the grant.
func (s *Server) handleCLIAuthApprove(w http.ResponseWriter, r *http.Request) {
	if !s.pro {
		http.Error(w, proNoticeCLILogin, http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	c, err := r.Cookie(cliAuthNonce)
	if err != nil || c.Value == "" ||
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostFormValue("nonce"))) != 1 {
		http.Error(w, "this approval page has gone stale — run `mimux mail login` again", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cliAuthNonce, Value: "", Path: "/cli/auth", MaxAge: -1})

	port, err := cliPort(r.PostFormValue("port"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	state, challenge := r.PostFormValue("state"), r.PostFormValue("challenge")
	if state == "" || challenge == "" {
		http.Error(w, "missing state or challenge", http.StatusBadRequest)
		return
	}
	expires, err := parseTokenExpiry(r.PostFormValue("expires"))
	if err != nil {
		http.Error(w, "That expiry date isn't valid.", http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(r.PostFormValue("label"))
	if label == "" {
		label = cliDefaultLabel
	}
	secret, tok, err := s.mintAPIToken(label, r.PostForm["scopes"], expires)
	if err != nil {
		slog.Error("cli auth: mint", "err", err) // never the secret or the hash
		http.Error(w, "Couldn't create the token.", http.StatusInternalServerError)
		return
	}
	code := auth.NewToken()
	s.putCLITicket(code, cliTicket{
		secret: secret, label: tok.Label, scopes: tok.Scopes,
		challenge: challenge, expiresAt: time.Now().Add(cliCodeTTL),
	})

	// --no-browser: there is no loopback listener to redirect to, so the code
	// goes on the page for the human to paste back into the terminal.
	if r.PostFormValue("nb") == "1" {
		s.renderRequest(w, r, "cli_auth", map[string]any{
			"CSRF": auth.EnsureCSRF(w, r, s.secure),
			"Code": code, "Label": tok.Label, "Pro": s.proView(),
		})
		return
	}
	http.Redirect(w, r, "http://"+cliLoopbackHost+":"+strconv.Itoa(port)+
		"/callback?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code),
		http.StatusSeeOther)
}

// handleCLIExchange trades the one-shot code plus the PKCE verifier for the
// token. Outside the auth group and outside the CSRF check: it reads no cookie
// and carries its own credential, exactly like the extension mounts.
func (s *Server) handleCLIExchange(w http.ResponseWriter, r *http.Request) {
	var req struct{ Code, Verifier string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	t, ok := s.takeCLITicket(req.Code)
	if !ok {
		http.Error(w, "unknown or already-collected code", http.StatusNotFound)
		return
	}
	if time.Now().After(t.expiresAt) {
		http.Error(w, "that code has expired", http.StatusUnauthorized)
		return
	}
	sum := sha256.Sum256([]byte(req.Verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(got), []byte(t.challenge)) != 1 {
		http.Error(w, "the verifier does not match the challenge", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token": t.secret, "label": t.label, "scopes": strings.Fields(t.scopes),
	})
}
