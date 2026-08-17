package mail

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/mattmezza/mimux/internal/config"
)

// fakeSession is a minimal go-smtp Session that requires PLAIN auth
// ("user"/"pass") and records the last delivered message.
type fakeSession struct {
	authed  bool
	from    string
	to      []string
	dataStr string
}

func (s *fakeSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *fakeSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, username, password string) error {
		if username != "user@example.com" || password != "pass" {
			return errors.New("bad credentials")
		}
		s.authed = true
		return nil
	}), nil
}

func (s *fakeSession) Mail(from string, _ *smtp.MailOptions) error {
	if !s.authed {
		return smtp.ErrAuthRequired
	}
	s.from = from
	return nil
}

func (s *fakeSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	if !s.authed {
		return smtp.ErrAuthRequired
	}
	s.to = append(s.to, to)
	return nil
}

func (s *fakeSession) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.dataStr = string(b)
	return nil
}

func (s *fakeSession) Reset()        {}
func (s *fakeSession) Logout() error { return nil }

// TestSMTPSendOverClient drives sendOverClient (PLAIN auth + envelope +
// data) against a real go-smtp server listening on a loopback port —
// dialSMTP's TLS wrapping is untestable without real certs, so this covers
// everything below it.
func TestSMTPSendOverClient(t *testing.T) {
	sess := &fakeSession{}
	be := smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) { return sess, nil })
	srv := smtp.NewServer(be)
	srv.Domain = "localhost"
	srv.AllowInsecureAuth = true

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := smtp.NewClient(conn)

	cfg := config.Account{Name: "Test", Email: "user@example.com", Password: "pass"}
	raw := []byte("Subject: hi\r\n\r\nBody\r\n")
	if err := sendOverClient(c, cfg, "", "user@example.com", []string{"to@example.com"}, raw); err != nil {
		t.Fatalf("sendOverClient: %v", err)
	}

	if !sess.authed {
		t.Error("server never saw a successful auth")
	}
	if sess.from != "user@example.com" {
		t.Errorf("MAIL FROM = %q", sess.from)
	}
	if len(sess.to) != 1 || sess.to[0] != "to@example.com" {
		t.Errorf("RCPT TO = %v", sess.to)
	}
	if !strings.Contains(sess.dataStr, "Body") {
		t.Errorf("DATA = %q", sess.dataStr)
	}
}

// TestSMTPAuthFailure asserts a wrong password surfaces as an error rather
// than silently "succeeding".
func TestSMTPAuthFailure(t *testing.T) {
	sess := &fakeSession{}
	be := smtp.BackendFunc(func(c *smtp.Conn) (smtp.Session, error) { return sess, nil })
	srv := smtp.NewServer(be)
	srv.Domain = "localhost"
	srv.AllowInsecureAuth = true

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := smtp.NewClient(conn)

	cfg := config.Account{Name: "Test", Email: "user@example.com", Password: "wrong"}
	err = sendOverClient(c, cfg, "", "user@example.com", []string{"to@example.com"}, []byte("Subject: hi\r\n\r\nBody\r\n"))
	if err == nil {
		t.Fatal("expected an auth error, got nil")
	}
}
