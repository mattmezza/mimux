//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mattmezza/mimux/internal/auth"
	"github.com/mattmezza/mimux/internal/store"
)

// `mimux mail webhooks listen` — the local half of webhooks. It holds the
// NDJSON stream from GET /api/v1/webhooks/listen open and re-POSTs every event
// to a URL on this machine, signed exactly the way production signs, so a
// receiver can be written and verified against a laptop before anything is
// deployed.
//
// It is not an endpoint. Nothing is registered, nothing is stored, and events
// that fire while this is not connected are not queued anywhere — they are
// gone. That is the price of needing no configuration at all, and it is the
// first thing the command prints.

// listenBackoff is the pause before reconnecting a dropped stream. Long enough
// not to hammer a mimux that is restarting, short enough that a `make dev`
// restart is barely noticed.
const listenBackoff = 3 * time.Second

// whsecPrefix marks the ephemeral signing secret this command mints. Borrowed
// from Stripe's CLI on purpose: it is the string a receiver's config already
// calls a webhook secret, and half the people running this have seen it before.
const whsecPrefix = "whsec_"

// execTimeout bounds one -execute run. A script that hangs must not wedge the
// loop forever; anything slower than this is not an event handler.
const execTimeout = 60 * time.Second

// webhookSubcommands is the second level of `mimux mail webhooks <verb>`. One
// entry today; the table is here so the next one is a line rather than a
// re-shape of the dispatch.
var webhookSubcommands = []cliCommand{
	{"listen", "-forward-to <url> | -execute <command>", "webhooks:manage", "Stream live events to a local URL (signed like production), or run a shell command per event with the payload on stdin.", cliWebhooksListen},
}

func cliWebhooks(c *cliClient, args []string) error {
	if len(args) == 0 {
		return usageError{"want a subcommand: " + strings.Join(webhookSubverbs(), ", ")}
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		c.printf("usage: mimux mail webhooks <subcommand> [flags]\n\n")
		for _, sub := range webhookSubcommands {
			c.printf("  %s %s\n    %s\n", sub.name, sub.args, sub.help)
		}
		return nil
	}
	for _, sub := range webhookSubcommands {
		if sub.name == args[0] {
			return sub.run(c, args[1:])
		}
	}
	return usageError{"unknown subcommand " + strconv.Quote(args[0]) + " (want: " + strings.Join(webhookSubverbs(), ", ") + ")"}
}

func webhookSubverbs() []string {
	out := make([]string, len(webhookSubcommands))
	for i, sub := range webhookSubcommands {
		out[i] = sub.name
	}
	return out
}

func cliWebhooksListen(c *cliClient, args []string) error {
	fs := c.flags("webhooks listen")
	forward := fs.String("forward-to", "", "local URL to POST each event to")
	program := fs.String("execute", "", "shell command to run per event, payload on stdin")
	events := fs.String("events", "", "only these events, comma-separated (default: every event)")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	if (*forward == "") == (*program == "") {
		return usageError{"exactly one of -forward-to or -execute is required, e.g. -forward-to http://localhost:3000/hooks/mimux"}
	}
	if *forward != "" && !strings.HasPrefix(*forward, "http://") && !strings.HasPrefix(*forward, "https://") {
		return usageError{"-forward-to wants an absolute http:// or https:// URL"}
	}
	filter := splitList(*events)
	if err := checkEventNames(filter); err != nil {
		return err
	}
	c.applyCreds()
	if strings.TrimSpace(c.token) == "" {
		return errors.New("not signed in — run `mimux mail login " + strings.TrimSuffix(c.base, "/") + "`, or set MIMUX_TOKEN to a token from Settings → API")
	}

	which := "every event"
	if len(filter) > 0 {
		which = strings.Join(filter, ", ")
	}
	var do func(context.Context, liveEvent)
	if *program != "" {
		// No secret in this mode: HMAC proves who sent an HTTP request, and a
		// process this command spawns itself has nothing to prove.
		c.printf("listening for: %s — executing %s per event (payload on stdin)\n", which, *program)
		do = func(ctx context.Context, ev liveEvent) { c.execute(ctx, *program, ev) }
	} else {
		// A fresh secret per run, never stored and never sent to the server: this
		// stream is signed by whoever is forwarding, and nothing else ever needs to
		// know it. Put it in the receiver's config for as long as this runs.
		secret := whsecPrefix + auth.NewToken()
		c.printf("listening for: %s — forwarding to %s\n", which, *forward)
		c.printf("signing secret: %s\n", secret)
		c.printf("Verify it exactly as you verify production: HMAC-SHA256 over \"<t>.<body>\".\n")
		do = func(ctx context.Context, ev liveEvent) { c.deliver(ctx, *forward, secret, ev) }
	}
	c.printf("Nothing is queued while this is not connected — events that fire meanwhile are lost.\n")
	c.printf("Ctrl-C to stop.\n\n")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return c.listenLoop(ctx, filter, do)
}

// checkEventNames rejects a typo before the stream opens: the server would
// silently filter it out (see store.ValidWebhookEvents) and the listener would
// just sit there receiving nothing.
func checkEventNames(events []string) error {
	valid := make([]string, len(store.WebhookEvents))
	for i, w := range store.WebhookEvents {
		valid[i] = w.ID
	}
	for _, e := range events {
		known := false
		for _, v := range valid {
			if v == e {
				known = true
				break
			}
		}
		if !known {
			return usageError{"unknown event " + strconv.Quote(e) + " (want: " + strings.Join(valid, ", ") + ")"}
		}
	}
	return nil
}

// listenLoop keeps one stream open, reconnecting after a pause when it drops. A
// stream that ends by itself is a mimux that restarted or a proxy that timed
// out, and picking it back up is what anyone watching wants; a refusal that
// will not heal on its own (a rejected token, a lapsed licence, a missing
// scope) stops instead of retrying it every few seconds forever.
func (c *cliClient) listenLoop(ctx context.Context, events []string, do func(context.Context, liveEvent)) error {
	for {
		retry, err := c.stream(ctx, events, do)
		if ctx.Err() != nil {
			c.printf("\nstopped\n")
			return nil
		}
		if !retry {
			return err
		}
		if err != nil {
			_, _ = fmt.Fprintf(c.errw, "stream dropped: %v — reconnecting in %s\n", err, listenBackoff)
		} else {
			_, _ = fmt.Fprintf(c.errw, "stream ended — reconnecting in %s\n", listenBackoff)
		}
		select {
		case <-ctx.Done():
			c.printf("\nstopped\n")
			return nil
		case <-time.After(listenBackoff):
		}
	}
}

// stream opens one connection and hands each event to do — a forward or an
// exec, the loop does not care — until it ends. The bool says whether
// reconnecting is worth trying.
func (c *cliClient) stream(ctx context.Context, events []string, do func(context.Context, liveEvent)) (bool, error) {
	path := "/api/v1/webhooks/listen"
	if len(events) > 0 {
		path += "?events=" + url.QueryEscape(strings.Join(events, ","))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.base, "/")+path, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := listenClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		return true, fmt.Errorf("couldn't reach %s: %w (is mimux running?)", c.base, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		return false, c.apiFailure(res, raw)
	}

	dropped := int64(0)
	dec := json.NewDecoder(res.Body)
	for {
		var ev liveEvent
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				return true, nil
			}
			return true, err
		}
		switch ev.Type {
		case "ping":
			continue // keepalive
		case "error":
			// The server closing the stream on purpose — a licence that lapsed
			// while this was connected. Reconnecting would just repeat it.
			return false, errors.New(ev.Message)
		}
		if ev.Dropped > dropped {
			c.printf("… %d event(s) dropped: this listener fell behind\n", ev.Dropped-dropped)
			dropped = ev.Dropped
		}
		do(ctx, ev)
	}
}

// listenClient is the stream's own transport. No Client.Timeout: that covers
// reading the body, so the two-minute one on the shared client would cut every
// stream off after two minutes. Dial and handshake are still bounded, because
// an unreachable host has to fail rather than hang.
func listenClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}}
}

// deliver POSTs one streamed event to the local receiver with the same three
// headers production sends, over the same bytes, signed with `t` stamped now —
// so a receiver that verifies this verifies the real thing unchanged.
//
// No retry. A receiver you are staring at does not need a ladder, and a silent
// retry would hide the failure this command exists to show you.
func (c *cliClient) deliver(ctx context.Context, to, secret string, ev liveEvent) {
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, to, bytes.NewReader(ev.Payload))
	if err != nil {
		c.printf("%-18s %s\n", ev.Event, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mimux-webhooks/1")
	req.Header.Set("X-Mimux-Event", ev.Event)
	req.Header.Set("X-Mimux-Delivery-Id", ev.ID)
	req.Header.Set("X-Mimux-Signature", signature(secret, time.Now().Unix(), string(ev.Payload)))
	res, err := c.http.Do(req)
	ms := time.Since(started).Milliseconds()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		c.printf("%-18s %-6s %5dms  %v\n", ev.Event, "-", ms, err)
		return
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10)) // so the connection is reused
	c.printf("%-18s %-6d %5dms\n", ev.Event, res.StatusCode, ms)
}

// execute runs one streamed event through a command: payload on stdin, event
// type and delivery id in the environment, stdout/stderr flowing straight to
// this command's own. Via `sh -c`, so `-execute 'jq .'` and pipes work — safe,
// because the command string is the operator's own argv, and event data only
// ever reaches the process on stdin, never interpolated into the command line.
//
// Serial on purpose: the server's drop accounting is the backpressure, so a
// slow script just shows up as the dropped counter. Same no-retry stance as
// deliver — a non-zero exit is printed, not retried.
func (c *cliClient) execute(ctx context.Context, command string, ev liveEvent) {
	started := time.Now()
	tctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, "/bin/sh", "-c", command)
	cmd.Stdin = bytes.NewReader(ev.Payload)
	cmd.Stdout = c.out
	cmd.Stderr = c.errw
	cmd.Env = append(os.Environ(), "MIMUX_EVENT="+ev.Event, "MIMUX_DELIVERY_ID="+ev.ID)
	err := cmd.Run()
	ms := time.Since(started).Milliseconds()
	switch {
	case ctx.Err() != nil: // Ctrl-C mid-run, not the script's fault
	case err == nil:
		c.printf("%-18s %-6s %5dms\n", ev.Event, "exit 0", ms)
	case tctx.Err() != nil:
		c.printf("%-18s %-6s %5dms  killed after %s\n", ev.Event, "-", ms, execTimeout)
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			c.printf("%-18s %-6s %5dms\n", ev.Event, fmt.Sprintf("exit %d", ee.ExitCode()), ms)
			return
		}
		c.printf("%-18s %-6s %5dms  %v\n", ev.Event, "-", ms, err)
	}
}
