// SPDX-License-Identifier: AGPL-3.0-only
package server

import "github.com/mattmezza/mimux/internal/ext"

// Where the settings pages send someone who wants the pro layer. The pricing
// page is the front door of the existing checkout (www/src/pricing), and
// support@ is the address the site already publishes for questions and refunds
// — one address, so a reply doesn't depend on which page they came from.
const (
	proPricingURL  = "https://mimux.dev/pricing/"
	proContactMail = "support@mimux.dev"
)

// proInfo is what the pro-only settings sections need to know: whether this
// build can actually do the thing, and where to get one that can.
type proInfo struct {
	Active  bool // the pro layer is compiled in (an extension registered itself)
	BuyURL  string
	Contact string
}

// proView reports whether the pro layer is linked into this binary. That is the
// honest question for the settings screens: a free build stores webhooks, API
// tokens and a licence key and delivers, serves and verifies none of them.
//
// ponytail: build presence, not licence validity — the licence line on
// Settings → Licence already says what a pro build made of the key, and
// duplicating that verdict here would mean two sources of truth for it.
func (s *Server) proView() proInfo {
	return proInfo{Active: len(ext.All()) > 0, BuyURL: proPricingURL, Contact: proContactMail}
}
