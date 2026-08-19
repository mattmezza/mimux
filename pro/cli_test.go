//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// cliRun drives one invocation against a stub API, returning exit code,
// stdout and stderr. The stub is the contract: what the CLI sends is as much
// under test as what it prints.
func cliRun(t *testing.T, h http.HandlerFunc, stdin string, args ...string) (int, string, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var out, errw bytes.Buffer
	c := &cliClient{
		base: srv.URL, token: "mimux_pat_test",
		out: &out, errw: &errw, in: strings.NewReader(stdin),
		http: srv.Client(),
	}
	return c.dispatch(args), out.String(), errw.String()
}

func TestCLIList(t *testing.T) {
	var gotAuth, gotQuery string
	h := func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		writeList(w, []messageJSON{{ID: 7, Subject: "hello", From: addrJSON{Name: "Ada"}}}, "")
	}
	code, out, errOut := cliRun(t, h, "", "list", "-unread", "-limit", "5")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if gotAuth != "Bearer mimux_pat_test" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if !strings.Contains(gotQuery, "unread=true") || !strings.Contains(gotQuery, "limit=5") {
		t.Errorf("query = %q", gotQuery)
	}
	for _, want := range []string{"ID", "hello", "Ada", "u.."} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Flags must work after the positional query: the flag package stops at the
// first non-flag, which would fold `-json` into the search string.
func TestCLIFlagsAfterPositionals(t *testing.T) {
	var got searchRequest
	h := func(w http.ResponseWriter, r *http.Request) {
		if !decodeJSON(w, r, &got) {
			return
		}
		writeList(w, []messageJSON{}, "")
	}
	code, out, errOut := cliRun(t, h, "", "search", "is:unread", "from:ada", "-limit", "3", "-json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got.Query != "is:unread from:ada" || got.Limit != 3 {
		t.Errorf("request = %+v", got)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("-json was swallowed into the query: %q", out)
	}
}

// A `--` still ends flag parsing: the query language negates with a leading -.
func TestCLIDoubleDashEndsFlags(t *testing.T) {
	var got searchRequest
	h := func(w http.ResponseWriter, r *http.Request) {
		if !decodeJSON(w, r, &got) {
			return
		}
		writeList(w, []messageJSON{}, "")
	}
	if code, _, errOut := cliRun(t, h, "", "search", "--", "-from:noreply", "-is:starred"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got.Query != "-from:noreply -is:starred" {
		t.Errorf("query = %q", got.Query)
	}
}

func TestCLIJSONIsVerbatim(t *testing.T) {
	body := `{"data":[{"id":7}],"next_cursor":""}`
	h := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
	code, out, _ := cliRun(t, h, "", "list", "-json")
	if code != 0 || strings.TrimSpace(out) != body {
		t.Fatalf("exit %d, out %q", code, out)
	}
}

// A 403 must name the scope: an agent told "forbidden" cannot fix itself.
func TestCLIScopeError(t *testing.T) {
	h := func(w http.ResponseWriter, _ *http.Request) {
		apiError(w, http.StatusForbidden, "insufficient_scope", "This token is missing the mail:modify scope.")
	}
	code, _, errOut := cliRun(t, h, "", "star", "7")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut, "mail:modify") || !strings.Contains(errOut, "Settings") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestCLIUsageErrors(t *testing.T) {
	unreachable := func(w http.ResponseWriter, _ *http.Request) { t.Error("no request should have been made") }
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"unknown verb", "unknown command", []string{"frobnicate"}},
		{"trash without -yes", "-yes", []string{"move", "7", "trash"}},
		{"bad id", "must be a number", []string{"read", "seven"}},
		{"search with no query", "query is required", []string{"search"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := cliRun(t, unreachable, "", tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2 (stderr: %s)", code, errOut)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr = %q, want %q in it", errOut, tc.want)
			}
		})
	}
}

// No token is a usage problem, not a 401 to decode: it never reaches the wire.
func TestCLINoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("no request should have been made")
	}))
	defer srv.Close()
	var out, errw bytes.Buffer
	c := &cliClient{base: srv.URL, out: &out, errw: &errw, in: strings.NewReader(""), http: srv.Client()}
	if code := c.dispatch([]string{"accounts"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "MIMUX_TOKEN") {
		t.Errorf("stderr = %q", errw.String())
	}
}

// draft -in-reply-to derives recipients and subject the way draft_reply does
// for MCP callers, then saves — and sends nothing.
func TestCLIDraftReply(t *testing.T) {
	var posted draftRequest
	h := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/messages/7":
			writeJSON(w, messageJSON{ID: 7, Account: "a1", Subject: "Lunch",
				From: addrJSON{Address: "ada@example.test"}, To: []string{"me@example.test"}})
		case r.URL.Path == "/api/v1/accounts":
			writeList(w, []accountJSON{{Name: "a1", Email: "me@example.test"}}, "")
		case r.URL.Path == "/api/v1/drafts" && r.Method == "POST":
			if !decodeJSON(w, r, &posted) {
				return
			}
			writeJSONStatus(w, http.StatusCreated, draftJSON{ID: 3, Account: posted.Account,
				To: posted.To, Subject: posted.Subject, Body: posted.Body})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}
	code, out, errOut := cliRun(t, h, "yes please\n", "draft", "-in-reply-to", "7")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if posted.To != "ada@example.test" || posted.Subject != "Re: Lunch" || posted.Account != "a1" {
		t.Errorf("posted = %+v", posted)
	}
	if posted.Kind != "reply" || strings.TrimSpace(posted.Body) != "yes please" {
		t.Errorf("posted = %+v", posted)
	}
	if !strings.Contains(out, "NOT sent") {
		t.Errorf("output missing the not-sent warning:\n%s", out)
	}
}

// -dry-run prints the message and never reaches the send endpoint.
func TestCLISendDryRun(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected %s %s", r.Method, r.URL.Path) }
	code, out, errOut := cliRun(t, h, "", "send", "-to", "ada@example.test", "-subject", "hi", "-body", "there", "-dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "nothing was sent") || !strings.Contains(out, "ada@example.test") {
		t.Errorf("output = %q", out)
	}
}

// TestCLIWebhooksListenForwards is the whole verb end to end: a fake NDJSON
// stream on one side, a receiver on the other, and a forwarded request that has
// to verify against the secret the command minted.
//
// The stub closes the stream with an `error` line after the event, which is how
// the real server ends one on a licence lapse — it makes the loop return by
// itself, so nothing here has to race a cancel against a delivery in flight.
func TestCLIWebhooksListenForwards(t *testing.T) {
	type forwarded struct {
		head http.Header
		body string
	}
	recv := make(chan forwarded, 4)
	rx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- forwarded{r.Header.Clone(), string(b)}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer rx.Close()

	const payload = `{"id":"abc123","event":"message.received","created_at":"2026-08-19T10:00:00Z","data":{"subject":"hi"}}`
	queries := make(chan string, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries <- r.URL.RawQuery
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		_ = enc.Encode(liveEvent{Type: "ping"}) // keepalives are swallowed, not forwarded
		_ = enc.Encode(liveEvent{
			Type: "event", Event: "message.received", ID: "abc123",
			Payload: json.RawMessage(payload),
		})
		_ = enc.Encode(liveEvent{Type: "error", Message: "that is all"})
	}))
	defer up.Close()

	const secret = "whsec_testsecret_testsecret"
	var out, errw bytes.Buffer
	c := &cliClient{base: up.URL, token: "mimux_pat_test", out: &out, errw: &errw, http: rx.Client()}
	done := make(chan error, 1)
	go func() { done <- c.listenLoop(context.Background(), rx.URL, []string{"message.received"}, secret) }()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "that is all") {
			t.Fatalf("listenLoop = %v, want the server's closing error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream never ended")
	}

	select {
	case got := <-recv:
		if got.body != payload {
			t.Errorf("forwarded body was re-serialised:\n%s\nwant\n%s", got.body, payload)
		}
		verify(t, secret, got.head.Get("X-Mimux-Signature"), got.body)
		if got.head.Get("X-Mimux-Event") != "message.received" || got.head.Get("X-Mimux-Delivery-Id") != "abc123" {
			t.Errorf("forwarded headers = %v", got.head)
		}
	default:
		t.Fatal("nothing was forwarded")
	}
	if len(recv) != 0 {
		t.Error("the keepalive was forwarded as if it were an event")
	}
	if q := <-queries; q != "events=message.received" {
		t.Errorf("events filter not passed through: %q", q)
	}
	if !strings.Contains(out.String(), "message.received") || !strings.Contains(out.String(), "204") {
		t.Errorf("no per-event line printed:\n%s", out.String())
	}
}

// A refusal that will not heal — a rejected token, a lapsed licence — must stop
// rather than reconnect every few seconds forever.
func TestCLIWebhooksListenStopsOnRefusal(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiError(w, http.StatusPaymentRequired, "licence_expired", "The licence has expired.")
	}))
	defer up.Close()
	var out, errw bytes.Buffer
	c := &cliClient{base: up.URL, token: "mimux_pat_test", out: &out, errw: &errw, http: up.Client()}
	done := make(chan error, 1)
	go func() { done <- c.listenLoop(context.Background(), "http://127.0.0.1:1", nil, "whsec_x") }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "licence") {
			t.Fatalf("err = %v, want the licence failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a 402 was retried instead of stopping")
	}
}

func TestCLIWebhooksUsage(t *testing.T) {
	nope := func(w http.ResponseWriter, _ *http.Request) { t.Error("a usage error should not reach the API") }
	for _, args := range [][]string{{"webhooks"}, {"webhooks", "nonesuch"}, {"webhooks", "listen"}, {"webhooks", "listen", "-forward-to", "ftp://x"}} {
		if code, _, _ := cliRun(t, nope, "", args...); code != 2 {
			t.Errorf("%v exited %d, want 2", args, code)
		}
	}
	if code, out, _ := cliRun(t, nope, "", "webhooks", "help"); code != 0 || !strings.Contains(out, "listen") {
		t.Errorf("webhooks help = %d:\n%s", code, out)
	}
}
