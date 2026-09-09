// SPDX-License-Identifier: AGPL-3.0-only
package mail

import (
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestDecodeHeader(t *testing.T) {
	tests := []struct{ name, input, want string }{
		{"windows 1252 quoted printable", "=?Windows-1252?Q?Mise_=E0_jour_de_notre_syst=E8me_de_r=E9servation?=", "Mise à jour de notre système de réservation"},
		{"utf8 quoted printable", "Mise =?UTF-8?Q?=C3=A0?= jour", "Mise à jour"},
		{"utf8 base64", "=?UTF-8?B?U3ViamVjdCDwn5KMIT8=?=", "Subject 💌!?"},
		{"adjacent encoded words", "=?UTF-8?Q?Mise_=C3=A0?= =?UTF-8?Q?_jour?=", "Mise à jour"},
		{"plain", "Quarterly update", "Quarterly update"},
		{"malformed", "=?UTF-8?Q?missing-terminator", "=?UTF-8?Q?missing-terminator"},
		{"unknown charset", "prefix =?x-mimux-unknown?Q?caf=E9?= suffix", "prefix =?x-mimux-unknown?Q?caf=E9?= suffix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeHeader(tt.input); got != tt.want {
				t.Errorf("decodeHeader(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMessageFromBufferDecodesEnvelopeWords(t *testing.T) {
	buf := &imapclient.FetchMessageBuffer{Envelope: &imap.Envelope{
		Subject: "=?Windows-1252?Q?Mise_=E0_jour?=",
		From:    []imap.Address{{Name: "=?UTF-8?B?Sm9zw6kgTcO8bGxlcg==?=", Mailbox: "jose", Host: "example.com"}},
	}}
	msg := messageFromBuffer("work", 7, buf, "")
	if msg.Subject != "Mise à jour" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	if msg.FromName != "José Müller" {
		t.Errorf("FromName = %q", msg.FromName)
	}
	if msg.FromAddress != "jose@example.com" {
		t.Errorf("FromAddress = %q", msg.FromAddress)
	}
}
