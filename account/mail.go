// SPDX-License-Identifier: LicenseRef-Elastic-2.0
package main

import (
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// mailer is a func, not an interface: one real implementation, and tests swap
// it for a closure that appends to a slice.
type mailer func(to, subject, body string) error

// smtpSender uses net/smtp, which negotiates STARTTLS when the server offers
// it. ponytail: no implicit-TLS (port 465) support — use the submission port
// 587, every provider speaks it. Add a tls.Dial branch if that ever changes.
func smtpSender(c config) mailer {
	return func(to, subject, body string) error {
		addr := net.JoinHostPort(c.smtpHost, c.smtpPort)
		var auth smtp.Auth
		if c.smtpUser != "" {
			auth = smtp.PlainAuth("", c.smtpUser, c.smtpPass, c.smtpHost)
		}
		msg, err := buildMessage(c.smtpFrom, to, subject, body)
		if err != nil {
			return err
		}
		return smtp.SendMail(addr, auth, c.smtpFrom, []string{to}, msg)
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
