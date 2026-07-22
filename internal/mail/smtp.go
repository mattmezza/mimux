package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/mattmezza/sm/internal/config"
)

// Send builds an RFC 5322 message for accountName and delivers it over SMTP,
// then — unless the account is Gmail, which auto-saves outgoing mail —
// appends it to the account's Sent folder and resyncs it. Returns the
// generated Message-ID.
func (m *Manager) Send(ctx context.Context, accountName string, in ComposeInput) (string, error) {
	a := m.accounts[accountName]
	if a == nil {
		return "", fmt.Errorf("unknown account %q", accountName)
	}
	rcpts := dedupAddrs(in.To, in.Cc, in.Bcc)
	if len(rcpts) == 0 {
		return "", errors.New("no recipients")
	}
	raw, msgID, err := BuildMessage(a.cfg, in, time.Now())
	if err != nil {
		return "", fmt.Errorf("build message: %w", err)
	}
	bareRcpts := make([]string, len(rcpts))
	for i, r := range rcpts {
		bareRcpts[i] = bareAddr(r)
	}
	var token string
	if a.cfg.Auth == "oauth2" {
		if token, err = m.accessToken(ctx, accountName); err != nil {
			return "", err
		}
	}
	envFrom := bareAddr(a.cfg.Email)
	if in.From != "" {
		envFrom = bareAddr(in.From)
	}
	if err := smtpSend(a.cfg, token, envFrom, bareRcpts, raw); err != nil {
		return "", err
	}
	if a.cfg.Provider != "gmail" {
		if err := m.appendSent(ctx, a, raw); err != nil {
			slog.Warn("smtp: append to sent failed", "account", accountName, "err", err)
		}
	}
	return msgID, nil
}

// appendSent stores a copy of a just-sent message in the account's Sent
// folder (\Seen) and resyncs that folder so it shows up immediately. A
// missing Sent folder (not yet discovered by IMAP sync) is not an error —
// the message was already delivered.
func (m *Manager) appendSent(ctx context.Context, a *account, raw []byte) error {
	sent, err := m.st.FolderBySpecial(a.cfg.Name, "sent")
	if err != nil {
		return err
	}
	if sent == nil {
		return nil
	}
	return a.submit(ctx, func(c *imapclient.Client) error {
		cmd := c.Append(sent.Name, int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}})
		if _, err := cmd.Write(raw); err != nil {
			_ = cmd.Close()
			return err
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		if _, err := cmd.Wait(); err != nil {
			return err
		}
		changed, err := a.syncFolder(ctx, c, sent, c.Caps())
		if err == nil && changed {
			a.signalListChanged()
		}
		return err
	})
}

// dialSMTP connects to the account's SMTP host: implicit TLS on 465,
// STARTTLS otherwise (587 and everything else).
func dialSMTP(cfg config.Account) (*smtp.Client, error) {
	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	if cfg.SMTPPort == 465 {
		return smtp.DialTLS(addr, nil)
	}
	return smtp.DialStartTLS(addr, nil)
}

// smtpAuth is the pluggable auth seam, mirroring imap.go's authenticate:
// password today (tries PLAIN, falls back to LOGIN), OAuth2/XOAUTH2 slots in
// here later by switching on cfg.Auth.
func smtpAuth(c *smtp.Client, cfg config.Account, token string) error {
	switch cfg.Auth {
	case "", "password":
	case "oauth2":
		return c.Auth(newXOAUTH2Client(cfg.Email, token))
	default:
		return fmt.Errorf("smtp: unsupported auth mechanism %q", cfg.Auth)
	}
	switch {
	case c.SupportsAuth(sasl.Plain):
		return c.Auth(sasl.NewPlainClient("", cfg.Email, cfg.Password))
	case c.SupportsAuth(sasl.Login):
		return c.Auth(sasl.NewLoginClient(cfg.Email, cfg.Password))
	default:
		return errors.New("smtp: server offers neither PLAIN nor LOGIN auth")
	}
}

// sendOverClient authenticates an already-connected client and delivers raw
// to the SMTP envelope recipients to. Split out from smtpSend so tests can
// drive it against a plaintext fake server without a TLS dial.
func sendOverClient(c *smtp.Client, cfg config.Account, token, from string, to []string, raw []byte) error {
	if err := smtpAuth(c, cfg, token); err != nil {
		return fmt.Errorf("smtp: auth: %w", err)
	}
	if err := c.SendMail(from, to, bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("smtp: send: %w", err)
	}
	return c.Quit()
}

func smtpSend(cfg config.Account, token, from string, to []string, raw []byte) error {
	c, err := dialSMTP(cfg)
	if err != nil {
		return fmt.Errorf("smtp: dial %s:%d: %w", cfg.SMTPHost, cfg.SMTPPort, err)
	}
	defer func() { _ = c.Close() }()
	return sendOverClient(c, cfg, token, from, to, raw)
}
