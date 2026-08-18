// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// mailer is a func, not an interface: one real implementation, and tests swap
// it for a closure that appends to a slice.
type mailer func(to, subject, body string) error

// Three goes at a flaky link before giving up on a key somebody paid for.
const mailAttempts = 3

// Vars rather than consts only so the tests need not sit through them.
var (
	// One deadline for dial plus the whole conversation. A stalled submission
	// host used to hang the Stripe webhook until Stripe gave up and retried —
	// and the retry finds the event already claimed and sends nothing.
	smtpTimeout    = 30 * time.Second
	mailRetryDelay = 5 * time.Second
)

// dispatch sends in the background: every caller is on a request path where the
// far end (Stripe, or a person staring at /retrieve) must not wait on SMTP. ref
// is the licence id where there is one, for tying a failure back to a customer.
func (a *app) dispatch(ref, to, subject, body string) {
	a.mail.Add(1)
	go func() {
		defer a.mail.Done()
		for attempt := 1; ; attempt++ {
			err := a.send(to, subject, body)
			if err == nil {
				return
			}
			slog.Error("email failed", "ref", ref, "subject", subject, "attempt", attempt, "err", err)
			if attempt == mailAttempts {
				slog.Error("giving up on email — customer must use /retrieve", "ref", ref)
				return
			}
			time.Sleep(time.Duration(attempt) * mailRetryDelay)
		}
	}()
}

// smtpSender speaks the submission conversation by hand rather than calling
// smtp.SendMail, which cannot be given a timeout. STARTTLS is negotiated when
// the server offers it, as SendMail did. ponytail: no implicit-TLS (port 465)
// support — use the submission port 587, every provider speaks it. Add a
// tls.Dial branch if that ever changes.
func smtpSender(c config) mailer {
	return func(to, subject, body string) error {
		msg, err := buildMessage(c.smtpFrom, to, subject, body)
		if err != nil {
			return err
		}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(c.smtpHost, c.smtpPort), smtpTimeout)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		// Absolute: a server that accepts the connection and then goes quiet is
		// the failure that has no other bound.
		if err := conn.SetDeadline(time.Now().Add(smtpTimeout)); err != nil {
			return err
		}
		cl, err := smtp.NewClient(conn, c.smtpHost)
		if err != nil {
			return err
		}
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(&tls.Config{ServerName: c.smtpHost}); err != nil {
				return err
			}
		}
		if c.smtpUser != "" {
			if err := cl.Auth(smtp.PlainAuth("", c.smtpUser, c.smtpPass, c.smtpHost)); err != nil {
				return err
			}
		}
		if err := cl.Mail(c.smtpFrom); err != nil {
			return err
		}
		if err := cl.Rcpt(to); err != nil {
			return err
		}
		w, err := cl.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write(msg); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		return cl.Quit()
	}
}

// buildMessage assembles a plain-text message. Recipient and subject are
// rejected outright if they contain CR or LF — that is header injection, and
// the recipient address comes from user input on /retrieve.
func buildMessage(from, to, subject, body string) ([]byte, error) {
	if !validEmail(to) {
		return nil, fmt.Errorf("invalid recipient address")
	}
	if strings.ContainsAny(subject, "\r\n") {
		return nil, fmt.Errorf("invalid subject")
	}
	h := []string{
		"From: mimux <" + from + ">",
		"To: " + to,
		"Subject: " + subject,
		"Date: " + time.Now().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
	}
	return []byte(strings.Join(h, "\r\n") + "\r\n\r\n" +
		strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")), nil
}

func validEmail(s string) bool {
	if s == "" || len(s) > 254 || strings.ContainsAny(s, "\r\n") {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}
