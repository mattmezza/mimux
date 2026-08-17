package mail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mattmezza/mimux/internal/store"
)

// UnsubKind identifies how a message's List-Unsubscribe header can be acted on.
type UnsubKind string

const (
	UnsubNone     UnsubKind = ""          // no usable List-Unsubscribe header
	UnsubOneClick UnsubKind = "one-click" // RFC 8058: https URL + List-Unsubscribe-Post: One-Click
	UnsubLink     UnsubKind = "link"      // https/http URL, no one-click support — open in a new tab
	UnsubMailto   UnsubKind = "mailto"    // mailto: address only — send via SMTP
)

// UnsubInfo is the parsed, actionable form of a message's unsubscribe headers.
type UnsubInfo struct {
	Kind   UnsubKind
	URL    string // set for UnsubOneClick / UnsubLink
	Mailto string // set for UnsubMailto (raw "mailto:..." URI)
}

var listUnsubURLRe = regexp.MustCompile(`<([^>]+)>`)

// ParseListUnsubscribe parses the raw List-Unsubscribe and List-Unsubscribe-Post
// header values (RFC 2369 / RFC 8058) into an actionable UnsubInfo. Preference
// order: a one-click https URL, then any http(s) URL, then a mailto: address.
func ParseListUnsubscribe(header, postHeader string) UnsubInfo {
	header = strings.TrimSpace(header)
	if header == "" {
		return UnsubInfo{}
	}
	var httpURL, mailto string
	for _, m := range listUnsubURLRe.FindAllStringSubmatch(header, -1) {
		v := strings.TrimSpace(m[1])
		lower := strings.ToLower(v)
		switch {
		case httpURL == "" && (strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")):
			httpURL = v
		case mailto == "" && strings.HasPrefix(lower, "mailto:"):
			mailto = v
		}
	}
	oneClick := httpURL != "" && strings.HasPrefix(strings.ToLower(httpURL), "https://") &&
		strings.Contains(strings.ToLower(postHeader), "list-unsubscribe=one-click")
	switch {
	case oneClick:
		return UnsubInfo{Kind: UnsubOneClick, URL: httpURL}
	case httpURL != "":
		return UnsubInfo{Kind: UnsubLink, URL: httpURL}
	case mailto != "":
		return UnsubInfo{Kind: UnsubMailto, Mailto: mailto}
	default:
		return UnsubInfo{}
	}
}

// unsubHTTPClient is dedicated to one-click unsubscribe POSTs: short timeout,
// no cookie jar (never sends credentials), and a capped redirect chain.
var unsubHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

// UnsubscribeInfo resolves msg's List-Unsubscribe headers (via the cached
// parsed body — same cache the reading pane uses) into an actionable UnsubInfo.
func (m *Manager) UnsubscribeInfo(ctx context.Context, msg *store.Message) (UnsubInfo, error) {
	body, err := m.parsedBody(ctx, msg, false)
	if err != nil {
		return UnsubInfo{}, err
	}
	return ParseListUnsubscribe(body.listUnsubscribe, body.listUnsubscribePost), nil
}

// Unsubscribe performs the server-side half of an unsubscribe: a one-click
// POST, or an unsubscribe email sent through the normal SMTP send path.
// UnsubLink is handled entirely client-side (open the URL) and should never
// reach here.
func (m *Manager) Unsubscribe(ctx context.Context, msg *store.Message, info UnsubInfo) error {
	switch info.Kind {
	case UnsubOneClick:
		return postOneClickUnsubscribe(ctx, info.URL)
	case UnsubMailto:
		to, subject, body := parseMailtoUnsubscribe(info.Mailto)
		_, err := m.Send(ctx, msg.Account, ComposeInput{To: []string{to}, Subject: subject, Body: body})
		return err
	default:
		return fmt.Errorf("no server-side unsubscribe action for %q", info.Kind)
	}
}

// postOneClickUnsubscribe sends the RFC 8058 one-click POST. Only http(s) URLs
// ever reach here (ParseListUnsubscribe only ever fills UnsubOneClick.URL from
// an https:// match), but the scheme is checked again as defense in depth.
func postOneClickUnsubscribe(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return fmt.Errorf("unsubscribe: refusing non-https URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader("List-Unsubscribe=One-Click"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := unsubHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("unsubscribe: server returned %s", resp.Status)
	}
	return nil
}

// parseMailtoUnsubscribe splits a "mailto:addr?subject=...&body=..." URI (RFC
// 6068) into a recipient plus a sensible default subject/body when the header
// didn't specify them.
func parseMailtoUnsubscribe(mailto string) (to, subject, body string) {
	subject, body = "Unsubscribe", "Please unsubscribe me from this mailing list."
	u, err := url.Parse(mailto)
	if err != nil {
		return strings.TrimPrefix(mailto, "mailto:"), subject, body
	}
	to = u.Opaque
	if to == "" {
		to = u.Path
	}
	if q := u.Query(); q != nil {
		if s := q.Get("subject"); s != "" {
			subject = s
		}
		if b := q.Get("body"); b != "" {
			body = b
		}
	}
	return to, subject, body
}
