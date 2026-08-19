// SPDX-License-Identifier: AGPL-3.0-only
package server

// Where the settings pages send someone who wants the pro layer. The pricing
// page is the front door of the existing checkout (www/src/pricing), and
// support@ is the address the site already publishes for questions and refunds
// — one address, so a reply doesn't depend on which page they came from.
const (
	proPricingURL  = "https://mimux.dev/pricing/"
	proContactMail = "support@mimux.dev"
)

// What the free build says when something posts at a pro-only mint endpoint
// anyway — the plain-text twin of the banner the page renders in its place.
const (
	proNoticeTokens   = "API tokens are spent by mimux pro — the REST API, the MCP server and the terminal client. This build has none of them, so it will not mint a credential nothing here can use."
	proNoticeCLILogin = "`mimux mail` is part of mimux pro, and this build does not carry it. Signing in would mint a token with nothing to spend it on."
)

// proInfo is what the pro-only settings sections need to know: whether this
// build can actually do the thing, and where to get one that can.
type proInfo struct {
	Active  bool // the pro layer is compiled in (an extension registered itself)
	BuyURL  string
	Contact string
}

// proView is what a template gets: whether the pro layer is linked in (s.pro,
// captured at startup), and where to get a build that has it. That is the
// honest question for the settings screens: a free build stores webhooks, API
// tokens and a licence key and delivers, serves and verifies none of them.
//
// ponytail: build presence, not licence validity — the licence line on
// Settings → Licence already says what a pro build made of the key, and
// duplicating that verdict here would mean two sources of truth for it.
func (s *Server) proView() proInfo {
	return proInfo{Active: s.pro, BuyURL: proPricingURL, Contact: proContactMail}
}
