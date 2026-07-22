// Package account holds multi-account management; for now, provider presets.
package account

type Preset struct {
	IMAPHost string
	IMAPPort int
	SMTPHost string
	SMTPPort int
}

var Presets = map[string]Preset{
	"gmail":      {IMAPHost: "imap.gmail.com", IMAPPort: 993, SMTPHost: "smtp.gmail.com", SMTPPort: 587},
	"zoho":       {IMAPHost: "imap.zoho.com", IMAPPort: 993, SMTPHost: "smtp.zoho.com", SMTPPort: 465},
	"purelymail": {IMAPHost: "imap.purelymail.com", IMAPPort: 993, SMTPHost: "smtp.purelymail.com", SMTPPort: 465},
}
