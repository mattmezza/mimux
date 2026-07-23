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

// oauthProviders are the presets that support the OAuth2 Connect flow (see
// internal/mail/oauth.go). Password auth works for any provider.
var oauthProviders = map[string]bool{"gmail": true, "zoho": true}

// PresetNames returns the provider preset keys in a stable order, each flagged
// for whether it supports OAuth2, for the account-editor's provider select.
func PresetNames() []struct {
	Name  string
	OAuth bool
} {
	return []struct {
		Name  string
		OAuth bool
	}{
		{"gmail", true},
		{"zoho", true},
		{"purelymail", false},
	}
}

// SupportsOAuth reports whether a provider preset offers the OAuth2 Connect flow.
func SupportsOAuth(provider string) bool { return oauthProviders[provider] }
