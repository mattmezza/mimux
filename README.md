# mimux

![mimux](.github/cover.png)

A fast, keyboard-driven email client you run yourself. Point it at the mailboxes
you already have, and read, search, triage and send from one unified inbox in the
browser or as an installed app on your phone. Your mail, your credentials and
your machine — no account to create, no third party in the middle.

One `docker compose up`. One SQLite file holds everything. Under the hood it is a
single Go binary serving htmx + Alpine.js + Tailwind, talking IMAP and SMTP.

## Features

- **Unified inbox** across every configured account, plus a per-account view
- **IMAP IDLE sync** in the background, with polling fallback
- **Conversation threading**, unread counts, and a full folder tree
  (inbox/sent/drafts/archive/spam/trash) per account
- **Compose, reply, reply-all, and forward** over SMTP — plain text, rich text
  or Markdown — with local draft autosave, undo-send and scheduled send
- **Search** — instant local full-text search plus on-demand deep IMAP
  server search, streamed back over SSE as results arrive
- **Filters** — a rule engine (conditions → actions: move, label, forward,
  mark read, star, delete, notify) that runs on incoming mail
- **AI compose/reply and translate**, both optional and bring-your-own-key
  (OpenRouter, Google Translate)
- **OAuth2** login for Gmail and Zoho, alongside plain password/app-password auth
- **Keyboard-first** — a full shortcut set for navigating and triaging mail
  without leaving the home row (see below)
- **Installable PWA** with an offline fallback to your last-synced inbox
- **Notifications** for new mail when mimux isn't open — Web Push straight from
  your server, or a POST to an [ntfy](https://ntfy.sh) topic (see below)

## mimux pro — the automation layer

Everything above is free, forever. If something *other than a human* needs to
drive mimux, that is [mimux pro](LICENSING.md): a REST API
(self-documented at `/api/v1/openapi.json`, rendered at
[docs.mimux.dev](https://docs.mimux.dev)), an MCP server at `/api/mcp` so
agents can search, read, triage and draft (never send without a second,
explicit step), a `mimux mail` CLI over the same API for shells, scripts and
agents with no MCP client, and signed webhooks with retries and a delivery
log. Machine
access authenticates with scoped API tokens from **Settings → API** —
`mimux mail login <url>` gets one without pasting anything, by opening your
browser to approve it. A pro build runs 14 days without a key, and mail itself
never stops working, licensed or not. Keys at
[account.mimux.dev](https://account.mimux.dev).

## Screenshots

_(placeholder — drop PNGs/GIFs of the unified inbox, reading pane, and
compose here before publishing)_

## Quick start

```sh
docker compose up
```

Or install the binary — one command, checksum-verified, Linux and macOS on both
architectures:

```sh
curl -fsSL https://mimux.dev/install.sh | bash            # the free client
curl -fsSL https://mimux.dev/install.sh | bash -s -- pro  # + the automation layer
```

Either way it needs **zero configuration** to boot. Open http://localhost:8083 — the first
visit walks you through creating your admin account, then add your email
accounts and API keys from **Settings → Accounts / Integrations**. Everything is
stored in the SQLite DB; the only knobs outside it are the bootstrap env vars
below.

## Development

```sh
make setup   # air, golangci-lint, npm deps
make css     # tailwind watcher (separate terminal)
make dev     # hot-reloading server
make help    # everything else
```

## Keyboard shortcuts

| Key | Action | Key | Action |
|-----|--------|-----|--------|
| `j` / `k` | Next / previous message | `R` / `A` / `F` | Reply / reply all / forward |
| `o` / `Enter` | Open selected message | `c` | Compose |
| `Esc` | Back / close pane, dialog, or search | `/` | Focus search |
| `r` / `u` | Mark read / unread | `Tab` (in search) | Cycle search scope |
| `s` | Star / unstar selected | `Esc` (in search) | Clear search, back to inbox |
| `e` | Archive | `d` / `#` | Delete |
| `!` | Mark as spam | `?` | Toggle this shortcut list |
| `g i` | Go to inbox | `g s` | Go to starred |
| `g d` | Go to drafts | `g t` | Go to sent |
| `0` | Unified inbox | `1`–`9` | Nth account's inbox |

## Architecture

A single Go binary (`cmd/mimux`) serves everything: chi handlers render
`html/template` pages and htmx fragments (`internal/server`), a background
`internal/mail` manager owns one IMAP connection per account (IDLE + poll
sync, SMTP send, body sanitization), and `internal/store` persists
messages/folders/filters/sessions to SQLite. The browser side stays
dependency-light — htmx for server-driven updates, Alpine.js for small local
UI state (menus, forms), Tailwind for styling — so there's no JS build step
beyond the CSS pipeline. `internal/filter`, `internal/search`, `internal/ai`,
and `internal/translate` are self-contained feature packages mounted as
sub-routers.

`pro/` is the separately licensed automation layer. Every file in it carries
`//go:build pro`, so the default build excludes it from the build graph
entirely — `make build` produces a binary with none of it linked in, and
`make verify-free` proves that from the dependency graph rather than asking you
to trust it.

It binds to the client through `internal/ext` — one struct, `ext.Deps`, handing
it the mail manager, the store and the config — and is not allowed to import
`internal/server` at all (`make verify-boundary`). That rule is the reason there
is no speculative "mail engine" interface: anything `pro/` needs that currently
lives as a private method on `*server.Server` has to move down into
`internal/mail` or `internal/store`, where the HTML handler calls the same code.
Shared operations end up in the domain layer because a real caller needed them
there, not because someone guessed in advance what an API would want.

## Configuration

There is **no config file**. Bootstrap settings — the ones that can't live in
the DB — come from environment variables, each with a working default, so a
fresh install with zero env vars boots and runs. Everything else (accounts,
credentials, sync cadence, translate/AI keys, preferences) is edited in the
**Settings** GUI and stored in the SQLite DB.

| Env var | Default | Description |
|---------|---------|-------------|
| `MIMUX_DB` | `./data/mimux.db` | SQLite database path (created if absent) |
| `MIMUX_HOST` | `0.0.0.0` | Bind address |
| `MIMUX_PORT` | `8083` | Bind port |
| `MIMUX_BASE_URL` | `http://localhost:<port>` | Public URL — used for OAuth redirects and email links; `https://` enables Secure cookies |
| `MIMUX_SECRET` | *(auto)* | Session/CSRF signing secret. When unset it is generated once and persisted to a `secret` file next to the DB, so it stays stable across restarts |
| `MIMUX_AI_BASE_URL` | *(OpenRouter)* | Any OpenAI-compatible chat-completions endpoint — another provider, or a local runner (`http://llama:8080/v1`). Given a base, `/chat/completions` is appended. With this set the OpenRouter key becomes optional |

Accounts (name, email, provider preset, password or OAuth2 credentials, custom
IMAP/SMTP hosts, aliases), the sync cadence and message cap, and the Google
Translate / OpenRouter keys are all managed under **Settings → Accounts** and
**Settings → Integrations**. Use **Settings → Accounts → Backup & restore** to
export/import a portable JSON copy of all of it (it contains your passwords and
keys in plain text — keep it safe).

## OAuth setup (Gmail, Zoho)

For accounts using OAuth2 (choose **OAuth2** in the account editor):

1. **Create an OAuth client** in the provider console:
   - **Gmail** — [Google Cloud Console](https://console.cloud.google.com/) →
     APIs & Services → Credentials → *Create OAuth client ID* → *Web
     application*. Enable the Gmail API for the project. Requested scope:
     `https://mail.google.com/`.
   - **Zoho** — [Zoho API Console](https://api-console.zoho.com/) → *Add
     Client* → *Server-based Application*. Scopes:
     `ZohoMail.accounts.READ ZohoMail.messages.ALL`. mimux uses the `.com`
     region endpoints (adjust `internal/mail/oauth.go` for other regions).
2. **Set the authorized redirect URI** to `<MIMUX_BASE_URL>/oauth/callback`
   (e.g. `https://mail.example.com/oauth/callback`). It must match
   `MIMUX_BASE_URL` exactly.
3. In **Settings → Accounts**, add the account with auth **OAuth2** and paste
   the client ID / client secret; save it.
4. The account shows **Connect** (in the sidebar and the Accounts list) until
   authorized — click it to grant consent. Tokens are stored in the DB and
   refreshed automatically; the sync worker (re)starts on callback.

## Notifications

Off until you turn them on, in **Settings → Notifications**. Pick *when* first:

- **Off** (default) — nothing is ever sent and no permission is ever requested.
- **Only what my filter rules say** — a rule with the **Notify me** action
  fires one. Set those up under *Filters*.
- **Every new message in an inbox**.

Either way mimux only notifies about new mail arriving in an **inbox**: never your
own sent mail, never the backlog downloaded on a first sync, never a message
that was already read elsewhere, and never anything older than a day.

Then pick *how* — the two transports are independent and can both be on:

**Web Push** (Settings → Notifications → *Enable on this device*) delivers from
your own server, with the sender and subject encrypted end-to-end to that
browser. Nothing to configure: the VAPID key pair is generated on first use and
stored in the database. Requirements:

- **HTTPS with a real certificate.** Browsers refuse both service workers and
  push on an insecure origin, so `http://<lan-ip>:8083` will not work.
- **iPhone/iPad: install the web app first.** Safari only allows push for a web
  app added to the Home Screen (iOS 16.4+) — Share → *Add to Home Screen*, open
  mimux from that icon, and enable notifications there. It cannot work in a normal
  Safari tab, in any browser on iOS.
- The browser asks for permission **once**. If it's refused, the button can't
  ask again — reset Notifications for the site in the browser's own settings.

Each browser/device subscribes separately and is listed with a Remove button.
Signing out drops that device's subscription. A subscription the push service
reports as gone (404/410) is deleted automatically.

**ntfy** needs no permission, no HTTPS and no installed app: put a topic URL
(`https://ntfy.sh/<something-long-and-unguessable>`, or your own ntfy server) in
the box, install the ntfy app, and subscribe to the same topic. This is the
fallback when Web Push isn't available. Anyone who knows the topic name can read
the notifications, so self-host ntfy if the sender and subject are sensitive.

*Privacy:* Web Push payloads are encrypted end-to-end — the push service
(Apple/Google/Mozilla) relays ciphertext it cannot read. It does still see that
your device received a push, and when: metadata, not content. ntfy sees the
sender and subject in the clear unless you run it yourself.

## Important notes

- **Run behind HTTPS in production.** Cookies are only marked `Secure` when
  `MIMUX_BASE_URL` starts with `https://` — put mimux behind a reverse proxy
  (Caddy, nginx, Traefik) and set `MIMUX_BASE_URL` to the public URL. OAuth
  callbacks also depend on it matching the redirect URI exactly.
- **All state lives in one SQLite file** (`MIMUX_DB`, `/data/mimux.db` in Docker),
  next to an auto-generated `secret` file. Back the directory up and you've
  backed up everything: accounts, credentials, message cache, sessions,
  filters, saved searches, OAuth tokens, API keys. (Prefer the in-app
  **Backup & restore** export for a portable, human-readable copy.)
- **Accounts are managed in the GUI** (Settings → Accounts). Add/edit/remove
  takes effect immediately — no restart. Removing an account also deletes its
  downloaded folders/messages from the app; re-adding an account with the same
  name reattaches any mail still on the server on the next sync.
- **Single-user by design.** One admin user, created by the first-run wizard.
  Don't expose it to the internet without HTTPS and a strong password.
- **Privacy defaults:** remote images and all external resources in emails are
  blocked until you click *Load external content* (or allow a sender
  permanently). No CDNs, no analytics, no tracking — everything is bundled in
  the binary.
- **Gmail:** use OAuth2 (recommended) or an app password with `auth =
  "password"`. Gmail label display and the `label:` filter action are currently
  dormant — the upstream go-imap v2 library can't fetch `X-GM-LABELS`/`X-GM-THRID`
  yet; threading falls back to the standard JWZ algorithm, which works well.
- **Known limitations (v0.20):** drafts are local-only — autosaved to the
  SQLite DB, not appended to the IMAP Drafts folder, so they don't follow you
  to another client; offline mode is read-only (the service worker falls back
  to the last-synced inbox, and actions need the server); Gmail labels are
  dormant (see the Gmail note above).
- **Translate / AI are off until you add keys** (Settings → Integrations:
  Google Translate, OpenRouter). Both fail gracefully when unset.

## Free and paid

**The mail client is free, AGPL-3.0, and complete — forever.** Everything above
this line and everything in the feature list: multi-account unified inbox, sync,
threading, search, compose, filters, calendar, keyboard shortcuts, PWA, push.
Nothing in it is held back, time-limited, or behind a licence key, and nothing
will be moved out of it later.

**The paid layer is automation only** — a REST API, an MCP server for AI agents,
a CLI over that same API, and webhooks. The rule is simple: if a *human* drives
mimux, that is free; if something *else* drives it, that is the paid part. AI
compose is human-driven, so it stays free.

### Running pro

The pro layer ships as a separate image and separate binaries, because the free
ones do not contain it at all — `pro/` is excluded from that build graph by a
build tag, not disabled at runtime (`make verify-free` proves it on every CI
run). Swap the image and add your key:

```yaml
services:
  mimux:
    image: ghcr.io/mattmezza/mimux:pro   # or :v0.21-pro to stay on one minor
    environment:
      - MIMUX_LICENCE_KEY=mimuxlic1....  # from account.mimux.dev
```

Binaries are attached to each release as `mimux-pro-<os>-<arch>`. A pro build
runs 14 days without a key, and **mail itself never stops working** whether the
key is present, expired or absent — only the API, MCP and webhook endpoints
answer 402. `mimux licence status` prints exactly where you stand. Buy or
re-send a key at [account.mimux.dev](https://account.mimux.dev).

## Contributing and support

Issues yes, pull requests usually no — bug reports, questions and suggestions are
the most useful thing you can send. Support is GitHub issues, best effort, from
one person with a day job.

- [CONTRIBUTING.md](CONTRIBUTING.md) — how to report a bug (start with
  `make diagnose`), when a PR is worth opening, and what I will probably say no
  to.
- [SECURITY.md](SECURITY.md) — how to report a vulnerability. Please don't use a
  public issue.
- [CLA.md](CLA.md) — one page, relevant only for the rare accepted PR.

## Licence

The mail client — `cmd/`, `internal/`, `web/` — is
**[AGPL-3.0-only](LICENSE)**. The `pro/` directory, when it lands, is under the
[Elastic Licence 2.0](pro/LICENSE).

GitHub shows a single licence badge for the repository and it reads AGPL-3.0,
which does not describe the whole tree. **[LICENSING.md](LICENSING.md)** maps
which directory is under which licence, explains how the two halves combine
legally, and how to verify the split yourself:

```sh
make verify-free      # proves the free binary links zero ELv2 code
make verify-licence   # proves every file's SPDX header is on the right side
```

`mimux` is a trademark of Matteo Merola. The licences cover the code, not the
name — please rename your fork.
