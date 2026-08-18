# Security

mimux handles your mail and your mailbox credentials. Reports about either are
taken seriously. What follows is what you can expect, honestly stated: this is a
solo project built in evenings, not a company with a security team.

## Supported versions

Only the latest release gets fixes. There are no backports and no maintenance
branches. If you are running something older, upgrade first and check whether the
issue is still there.

## Reporting a vulnerability

**Do not open a public issue for a vulnerability.**

1. Preferred: open a private [GitHub security
   advisory](https://github.com/mattmezza/mimux/security/advisories/new).
2. Fallback, if you can't or won't use GitHub: email <security@mimux.dev>.

Include what you did, what happened, and what you think the impact is. A working
proof of concept moves things along enormously.

## What to expect

- **Acknowledgement within 7 days.** Usually much sooner, but 7 days is the
  number I'm willing to promise.
- **No SLA beyond that.** Fix timelines depend on severity and on how much of a
  week the day job leaves over. I'll tell you what I'm doing and when.
- **No bug bounty.** There is no money. There is genuine gratitude.
- **Credit in the advisory**, under whatever name you give me, unless you'd
  rather stay anonymous — just say so.

## Scope

**In scope:**

- Authentication and session handling
- CSRF protection
- The HTML sanitiser for email bodies (`internal/mail/sanitize.go`) — XSS,
  sanitiser bypass, script or style injection surviving into the reading pane
- Remote-content blocking (tracking pixels or external resources loading before
  you consent)
- Credential storage in the SQLite DB
- OAuth token handling and refresh
- The filter rule engine

**Out of scope:**

- Anything that requires the attacker to already be the admin user. mimux is
  single-user by design — one admin, created by the first-run wizard — so
  privilege escalation between users is not a category that exists here.
- Missing hardening headers with no demonstrated impact.
- Automated scanner output with no working proof of concept.
- Denial of service by an authenticated admin against their own instance.
- **Backup & restore exports credentials in plain text.** That is intentional,
  documented, and warned about in the README. It is a portable export of your own
  data, on your own machine.

## Known design tradeoffs

These are decisions, not bugs. Please don't report them as vulnerabilities.

- **Single-user by design.** One admin account. No roles, no tenants, no sharing.
- **Account credentials, OAuth tokens and API keys live in the SQLite DB**, not
  encrypted at rest. The threat model is a machine you control: if someone can
  read your database file, they can also read your session cookie and your mail.
  Back that file up somewhere safe.
- **Remote images are blocked by default**, but you can permanently allow a
  sender. Doing so is a choice you make per sender, and it does mean tracking
  pixels from that sender will load.
- **Drafts are local-only** — stored in the SQLite DB, never appended to the IMAP
  Drafts folder.

## Deployment

Run mimux behind HTTPS with a reverse proxy (Caddy, nginx, Traefik) and set
`MIMUX_BASE_URL` to the public `https://` URL — session cookies are only marked
`Secure` when that variable starts with `https://`.
