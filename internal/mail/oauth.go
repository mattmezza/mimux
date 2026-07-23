package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/emersion/go-sasl"
	"golang.org/x/oauth2"

	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/store"
)

// ErrNoToken is returned when an oauth2 account has no stored token yet —
// surfaced to the UI as an "authorize needed" state, never a hard error.
var ErrNoToken = errors.New("oauth2: no stored token; authorization needed")

// --- XOAUTH2 SASL client ---
//
// Gmail and Zoho want AUTH XOAUTH2, whose initial response is the literal
// "user=<email>\x01auth=Bearer <token>\x01\x01" (base64-encoded by the
// transport). go-sasl only ships OAUTHBEARER (RFC 7628), a different wire
// format, so we implement the ~15-line XOAUTH2 client by hand.
type xoauth2Client struct {
	username, token string
}

func newXOAUTH2Client(username, token string) sasl.Client {
	return &xoauth2Client{username: username, token: token}
}

func (c *xoauth2Client) Start() (mech string, ir []byte, err error) {
	return "XOAUTH2", xoauth2InitialResponse(c.username, c.token), nil
}

func (c *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	// A challenge here means the server rejected the token and sent a base64
	// JSON error; per the spec the client sends an empty response, but returning
	// the error surfaces the reason.
	return nil, fmt.Errorf("xoauth2: %s", strings.TrimSpace(string(challenge)))
}

func xoauth2InitialResponse(username, token string) []byte {
	return []byte("user=" + username + "\x01auth=Bearer " + token + "\x01\x01")
}

// --- OAuth2 provider config + token sourcing ---

// OAuthConfig builds the provider's oauth2.Config for an account. It is
// exported so the HTTP layer can drive the consent/callback flow.
func OAuthConfig(cfg config.Account, baseURL string) (*oauth2.Config, error) {
	c := &oauth2.Config{
		ClientID:     cfg.OAuth2ClientID,
		ClientSecret: cfg.OAuth2ClientSecret,
		RedirectURL:  strings.TrimRight(baseURL, "/") + "/oauth/callback",
	}
	switch cfg.Provider {
	case "gmail":
		c.Endpoint = oauth2.Endpoint{
			AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
			TokenURL: "https://oauth2.googleapis.com/token",
		}
		c.Scopes = []string{"https://mail.google.com/"}
	case "zoho":
		// Zoho is region-specific; .com is the default. See README OAuth setup.
		c.Endpoint = oauth2.Endpoint{
			AuthURL:  "https://accounts.zoho.com/oauth/v2/auth",
			TokenURL: "https://accounts.zoho.com/oauth/v2/token",
		}
		c.Scopes = []string{"ZohoMail.accounts.READ", "ZohoMail.messages.ALL"}
	default:
		return nil, fmt.Errorf("oauth2 not supported for provider %q", cfg.Provider)
	}
	return c, nil
}

// persistTokenSource refreshes via the wrapped source and writes any newly
// minted token back to the store so the refresh survives restarts.
type persistTokenSource struct {
	base    oauth2.TokenSource
	st      *store.Store
	account string
	lastAcc string
}

func (p *persistTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	if tok.AccessToken != p.lastAcc {
		p.lastAcc = tok.AccessToken
		_ = p.st.SaveToken(p.account, store.StoredToken{
			Access: tok.AccessToken, Refresh: tok.RefreshToken, Expiry: tok.Expiry,
		})
	}
	return tok, nil
}

// accessToken returns a currently-valid access token for an oauth2 account,
// refreshing and persisting it as needed. Returns ErrNoToken when the account
// has not been authorized.
func (m *Manager) accessToken(ctx context.Context, accountName string) (string, error) {
	a := m.account(accountName)
	if a == nil {
		return "", fmt.Errorf("unknown account %q", accountName)
	}
	cfg := a.cfg
	stored, err := m.st.GetToken(accountName)
	if err != nil {
		return "", err
	}
	if stored == nil || stored.Access == "" && stored.Refresh == "" {
		return "", ErrNoToken
	}
	oc, err := OAuthConfig(cfg, m.cfg.Server.BaseURL)
	if err != nil {
		return "", err
	}
	tok := &oauth2.Token{
		AccessToken:  stored.Access,
		RefreshToken: stored.Refresh,
		Expiry:       stored.Expiry,
		TokenType:    "Bearer",
	}
	src := &persistTokenSource{
		base:    oauth2.ReuseTokenSource(tok, oc.TokenSource(ctx, tok)),
		st:      m.st,
		account: accountName,
		lastAcc: stored.Access,
	}
	fresh, err := src.Token()
	if err != nil {
		return "", fmt.Errorf("oauth2 refresh: %w", err)
	}
	return fresh.AccessToken, nil
}
