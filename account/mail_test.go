// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// webhookRequest is the raw request the async tests drive by hand: they cannot
// go through post(), which joins the background sender before returning.
func webhookRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/stripe/webhook", strings.NewReader(body))
	r.Header.Set("Stripe-Signature", signed(t, body, time.Now()))
	return r
}

// TestWebhookDoesNotWaitForSMTP is the bug: a submission host that accepts the
// connection and then stalls used to hold the webhook open until Stripe gave up
// and retried — and the retry finds the event claimed and sends nothing. The
// mailer here never returns until the test says so, so if the handler still
// waits on it this test deadlocks rather than merely failing.
func TestWebhookDoesNotWaitForSMTP(t *testing.T) {
	a, sent := newTestApp(t)
	release := make(chan struct{})
	a.send = func(to, subject, body string) error {
		<-release
		sent.add(sentMail{to, subject, body})
		return nil
	}

	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, webhookRequest(t, checkoutEvent("evt_async", "subscription", planAnnual)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if n := sent.len(); n != 0 {
		t.Fatalf("emails = %d while the mailer is still blocked: the handler waited on it", n)
	}
	if n := countLicences(t, a); n != 1 {
		t.Fatalf("licences = %d, want the licence committed before the email", n)
	}

	// The mail is only deferred, not dropped.
	close(release)
	a.mail.Wait()
	if n := sent.len(); n != 1 {
		t.Fatalf("emails = %d after draining, want 1", n)
	}
	if !strings.Contains(sent.all()[0].body, keyPrefix+".") {
		t.Error("delivered email carries no licence key")
	}
}

// TestRetrieveDoesNotWaitForSMTP: same guarantee for the page a human is
// watching. It must also answer identically whether or not the address bought
// anything, which is only true while the send is off the response path.
func TestRetrieveDoesNotWaitForSMTP(t *testing.T) {
	a, sent := newTestApp(t)
	post(t, a, checkoutEvent("evt_seed_async", "subscription", planAnnual),
		signed(t, checkoutEvent("evt_seed_async", "subscription", planAnnual), time.Now()))
	sent.reset()

	release := make(chan struct{})
	a.send = func(to, subject, body string) error {
		<-release
		sent.add(sentMail{to, subject, body})
		return nil
	}

	r := httptest.NewRequest(http.MethodPost, "/retrieve", strings.NewReader("email=buyer@example.com&action=licence"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if sent.len() != 0 {
		t.Fatal("the page waited on the mailer")
	}

	close(release)
	a.mail.Wait()
	if sent.len() != 1 {
		t.Fatalf("emails = %d after draining, want 1", sent.len())
	}
}

// TestMailRetriesUntilItSticks: one flaky connection must not cost a paying
// customer their key.
func TestMailRetriesUntilItSticks(t *testing.T) {
	a, sent := newTestApp(t)
	var attempts atomic.Int32
	a.send = func(to, subject, body string) error {
		if attempts.Add(1) < mailAttempts {
			return errors.New("connection reset")
		}
		sent.add(sentMail{to, subject, body})
		return nil
	}

	a.dispatch("lic_1", "buyer@example.com", "Your mimux pro licence key", "body")
	a.mail.Wait()

	if got := attempts.Load(); got != mailAttempts {
		t.Errorf("attempts = %d, want %d", got, mailAttempts)
	}
	if sent.len() != 1 {
		t.Fatalf("emails = %d, want the retry to have landed", sent.len())
	}
}

// TestMailGivesUpBounded: a permanently broken mailer must stop, not retry for
// ever — an unbounded goroutine is a leak, and enough of them is the outage.
// Wait returning at all is the proof the goroutine ended.
func TestMailGivesUpBounded(t *testing.T) {
	a, sent := newTestApp(t)
	var attempts atomic.Int32
	a.send = func(string, string, string) error {
		attempts.Add(1)
		return errors.New("no route to host")
	}

	a.dispatch("lic_2", "buyer@example.com", "Your mimux pro licence key", "body")
	a.drainMail(10 * time.Second) // the same bounded wait shutdown uses

	if got := attempts.Load(); got != mailAttempts {
		t.Errorf("attempts = %d, want exactly %d", got, mailAttempts)
	}
	if sent.len() != 0 {
		t.Errorf("emails = %d, want none", sent.len())
	}
}

// smtpSink is enough of a submission server to answer smtpSender: greeting,
// EHLO with no extensions offered (so no STARTTLS, as against a plain relay),
// and a DATA body it hands back. ponytail: no TLS and no auth here — those are
// net/smtp's code, not ours; what is worth testing is that our hand-rolled
// conversation gets a message across intact.
func smtpSink(t *testing.T) (port string, delivered chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	delivered = make(chan string, 1)

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		say("220 sink ESMTP")
		var envelope, body strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			verb := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(verb, "EHLO"), strings.HasPrefix(verb, "HELO"):
				say("250 sink")
			case strings.HasPrefix(verb, "MAIL"), strings.HasPrefix(verb, "RCPT"):
				envelope.WriteString(strings.TrimSpace(line) + "\n")
				say("250 ok")
			case strings.HasPrefix(verb, "DATA"):
				say("354 go ahead")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimSpace(l) == "." {
						break
					}
					body.WriteString(l)
				}
				say("250 queued")
			case strings.HasPrefix(verb, "QUIT"):
				say("221 bye")
				delivered <- envelope.String() + "\n" + body.String()
				return
			default:
				say("250 ok")
			}
		}
	}()

	_, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port, delivered
}

// TestSMTPSenderDelivers runs the real client — the hand-rolled replacement for
// smtp.SendMail — against a local sink, so the conversation is exercised
// without a live relay and without sending anyone anything.
func TestSMTPSenderDelivers(t *testing.T) {
	port, delivered := smtpSink(t)
	send := smtpSender(config{smtpHost: "127.0.0.1", smtpPort: port, smtpFrom: "shop@mimux.dev"})

	if err := send("buyer@example.com", "Your mimux pro licence key", "line one\nline two"); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case got := <-delivered:
		for _, want := range []string{
			"MAIL FROM:<shop@mimux.dev>", "RCPT TO:<buyer@example.com>",
			"From: mimux <shop@mimux.dev>", "Subject: Your mimux pro licence key",
			"line one\r\nline two",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("delivered message is missing %q:\n%s", want, got)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing reached the sink")
	}

	// The header guards still hold on the way in.
	if err := send("victim@example.com\r\nBcc: someone@example.com", "s", "b"); err == nil {
		t.Error("smtpSender accepted an injected recipient")
	}
}

// TestSMTPSenderTimesOut: a host that accepts the connection and then says
// nothing must not hold the sender for ever. The deadline is dropped to
// something a test can wait out.
func TestSMTPSenderTimesOut(t *testing.T) {
	defer func(d time.Duration) { smtpTimeout = d }(smtpTimeout)
	smtpTimeout = 200 * time.Millisecond

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	defer func() { _ = ln.Close() }()
	stall := make(chan struct{})
	defer close(stall)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		// Accept and stall: never send the 220 greeting. Closed when the
		// listener goes, which is after the send has given up.
		<-stall
		_ = c.Close()
	}()

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	send := smtpSender(config{smtpHost: "127.0.0.1", smtpPort: port, smtpFrom: "from@mimux.dev"})

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- send("buyer@example.com", "subject", "body") }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a silent server produced a successful send")
		}
		t.Logf("gave up after %s: %v", time.Since(start).Round(time.Millisecond), err)
	case <-time.After(smtpTimeout + 10*time.Second):
		t.Fatal("smtpSender is still waiting on a silent server: the deadline is not being set")
	}
}
