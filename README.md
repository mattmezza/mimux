# SM — Simple Mail

Self-hosted, privacy-first web email client. A PWA served by a single Go
binary: htmx + Alpine.js + Tailwind on the front, IMAP/SMTP + SQLite on the
back. Runs as one Docker container — bring your own mailboxes, keep your own
data.

## Features

- **Unified inbox** across every configured account, plus a per-account view
- **IMAP IDLE sync** in the background, with polling fallback
- **Conversation threading**, unread counts, and a full folder tree
  (inbox/sent/drafts/archive/spam/trash) per account
- **Compose, reply, reply-all, and forward** over SMTP, with local draft
  autosave
- **Search** — instant local full-text search plus on-demand deep IMAP
  server search, streamed back over SSE as results arrive
- **Filters** — a rule engine (conditions → actions: move, label, forward,
  mark read, star, delete) that runs on incoming mail
- **AI compose/reply and translate**, both optional and bring-your-own-key
  (OpenRouter, Google Translate)
- **OAuth2** login for Gmail and Zoho, alongside plain password/app-password auth
- **Keyboard-first** — a full shortcut set for navigating and triaging mail
  without leaving the home row (see below)
- **Installable PWA** with an offline fallback to your last-synced inbox

## Screenshots

_(placeholder — drop PNGs/GIFs of the unified inbox, reading pane, and
compose here before publishing)_

## Quick start

```sh
cp config.example.toml config.toml   # edit: set server.secret at minimum
docker compose up
```

Open http://localhost:8080 — the first visit walks you through creating your
admin account. See [`config.example.toml`](config.example.toml) for every
account/provider option, or the full reference table below.

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

A single Go binary (`cmd/sm`) serves everything: chi handlers render
`html/template` pages and htmx fragments (`internal/server`), a background
`internal/mail` manager owns one IMAP connection per account (IDLE + poll
sync, SMTP send, body sanitization), and `internal/store` persists
messages/folders/filters/sessions to SQLite. The browser side stays
dependency-light — htmx for server-driven updates, Alpine.js for small local
UI state (menus, forms), Tailwind for styling — so there's no JS build step
beyond the CSS pipeline. `internal/filter`, `internal/search`, `internal/ai`,
and `internal/translate` are self-contained feature packages mounted as
sub-routers.

## Configuration

Config path comes from `$SM_CONFIG` env or `-config` flag (default: `./config.toml`).
See [`config.example.toml`](config.example.toml) for examples.

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `server.host` | string | `"0.0.0.0"` | Bind address |
| `server.port` | int | `8080` | Bind port |
| `server.base_url` | string | — | Public URL (https:// in production; enables Secure cookies) |
| `server.secret` | string | — | **Required**: 32+ char random key for sessions & CSRF (generate: `openssl rand -base64 32`) |
| `db.path` | string | `"./data/sm.db"` | SQLite database path |
| `accounts[].name` | string | — | Display name for the account |
| `accounts[].email` | string | — | Email address |
| `accounts[].provider` | string | — | Provider preset: `gmail`, `zoho`, `purelymail`, or omit for custom |
| `accounts[].auth` | string | `"password"` | Auth mode: `"password"` or `"oauth2"` |
| `accounts[].password` | string | — | Password/app-password (if `auth = "password"`) |
| `accounts[].oauth2_client_id` | string | — | OAuth2 client ID (if `auth = "oauth2"`) |
| `accounts[].oauth2_client_secret` | string | — | OAuth2 client secret (if `auth = "oauth2"`) |
| `accounts[].imap_host` | string | *preset* | IMAP host (auto-filled for preset providers) |
| `accounts[].imap_port` | int | *preset* | IMAP port (auto-filled for preset providers) |
| `accounts[].smtp_host` | string | *preset* | SMTP host (auto-filled for preset providers) |
| `accounts[].smtp_port` | int | *preset* | SMTP port (auto-filled for preset providers) |
| `translate.api_key` | string | `""` | Google Translate API key (empty = disabled) |
| `translate.target_language` | string | `"en"` | Target language code for translation (e.g., `"en"`, `"es"`, `"fr"`) |
| `ai.openrouter_api_key` | string | `""` | OpenRouter API key (empty = AI features disabled) |
| `ai.model` | string | `"anthropic/claude-sonnet-4-6"` | AI model via OpenRouter |
| `sync.poll_interval` | duration | `"5m"` | IMAP poll interval when IDLE unavailable (e.g., `"5m"`, `"30s"`) |
| `sync.max_messages_per_sync` | int | `500` | Max messages per account per sync cycle |

## OAuth setup (Gmail, Zoho)

For accounts with `auth = "oauth2"`:

1. **Create an OAuth client** in the provider console:
   - **Gmail** — [Google Cloud Console](https://console.cloud.google.com/) →
     APIs & Services → Credentials → *Create OAuth client ID* → *Web
     application*. Enable the Gmail API for the project. Requested scope:
     `https://mail.google.com/`.
   - **Zoho** — [Zoho API Console](https://api-console.zoho.com/) → *Add
     Client* → *Server-based Application*. Scopes:
     `ZohoMail.accounts.READ ZohoMail.messages.ALL`. SM uses the `.com`
     region endpoints (see the Zoho note in `config.example.toml`).
2. **Set the authorized redirect URI** to `<base_url>/oauth/callback`
   (e.g. `https://mail.example.com/oauth/callback`). It must match
   `server.base_url` exactly.
3. Put `oauth2_client_id` / `oauth2_client_secret` in the account block and
   set `auth = "oauth2"`.
4. Start SM and sign in. The account shows **Connect account** in the sidebar
   until authorized — click it (or visit `/oauth/<name>/start`) to grant
   consent. Tokens are stored in the DB and refreshed automatically; the sync
   worker (re)starts on callback.

## Important notes

- **Set `server.secret` before first run** (`openssl rand -base64 32`). It signs
  sessions and CSRF tokens; changing it later logs everyone out.
- **Run behind HTTPS in production.** Cookies are only marked `Secure` when
  `server.base_url` starts with `https://` — put SM behind a reverse proxy
  (Caddy, nginx, Traefik) and set `base_url` to the public URL. OAuth callbacks
  also depend on `base_url` matching the redirect URI exactly.
- **All state lives in one SQLite file** (`db.path`, `/data/sm.db` in Docker).
  Back that file up and you've backed up everything: message cache, sessions,
  filters, saved searches, OAuth tokens. Deleting it is safe-ish — SM re-syncs
  from IMAP — but you lose filters, tokens, and your admin user.
- **Accounts are configured in `config.toml` only** — there is no accounts UI.
  Adding/changing an account requires a restart. OAuth tokens, however, are
  stored in the DB after the one-time browser consent flow.
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
- **Known limitations (v0.1):** compose is plain-text; drafts are local-only
  (not synced to the IMAP Drafts folder); notifications arrive over SSE while a
  tab is open (no Web Push); offline mode is read-only.
- **Translate / AI are off until you add keys** (`translate.api_key`,
  `ai.openrouter_api_key`). Both fail gracefully when unset.

## License

[MIT](LICENSE)
