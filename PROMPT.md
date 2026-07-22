# SM — Simple Mail

## Project Overview

Build **SM** (Simple Mail), a self-hosted, privacy-first web-based email client. It is a PWA served by a Go backend, styled with Tailwind CSS, and driven by htmx + Alpine.js (no React, no heavy JS frameworks). It connects to multiple email accounts via IMAP/SMTP and runs as a single Docker container.

The repo lives at **github.com/mattmezza/sm**. The project must be production-quality from day one: clean code, tested, documented, CI/CD baked in, and a joy to use.

---

## Architecture

```
┌─────────────────────────────────────┐
│           Browser (PWA)             │
│  htmx + Alpine.js + Tailwind CSS   │
│  Service Worker · Manifest          │
└──────────────┬──────────────────────┘
               │ HTTP/SSE
┌──────────────▼──────────────────────┐
│           Go Backend                │
│  Chi router · Session auth          │
│  IMAP sync engine (go-imap v2)      │
│  SMTP sender (go-message)           │
│  SQLite (metadata cache + rules)    │
│  Background workers (IDLE/poll)     │
└──────┬───────────┬──────────────────┘
       │           │
  IMAP/SMTP    External APIs
  (providers)  (OpenRouter, Google Translate)
```

### Key technology choices

| Layer | Choice | Why |
|---|---|---|
| Language | Go 1.23+ | Best IMAP/SMTP ecosystem, easy Docker builds, fast compilation |
| IMAP | `github.com/emersion/go-imap/v2` + `go-message` | Most mature Go IMAP library, actively maintained |
| SMTP | `github.com/emersion/go-sasl`, `go-smtp`, `go-message` | Same author, interoperable |
| OAuth2 | `golang.org/x/oauth2` | Gmail requires OAuth2, Zoho supports it |
| DB | SQLite via `modernc.org/sqlite` (pure Go, no CGo) | Zero-dep, single-file, fast for metadata caching |
| HTTP | `github.com/go-chi/chi/v5` | Lightweight, composable, middleware-friendly |
| Templates | Go `html/template` with a custom component system | Server-rendered HTML for htmx |
| Frontend | Tailwind CSS 4 + htmx 2.x + Alpine.js 3.x | Lightweight, hypermedia-driven, no build step needed |
| Auth | Session-based with secure cookies + argon2id passwords | Simple, no JWT complexity |
| Config | TOML file + env var overrides | Human-readable, twelve-factor friendly |

### Directory structure

```
sm/
├── cmd/
│   └── sm/
│       └── main.go              # Entrypoint
├── internal/
│   ├── server/                  # HTTP server, routes, middleware
│   ├── auth/                    # Authentication, sessions
│   ├── mail/                    # IMAP sync engine, SMTP sender
│   │   ├── imap.go
│   │   ├── smtp.go
│   │   ├── thread.go            # JWZ threading algorithm
│   │   ├── sanitize.go          # HTML email sanitization
│   │   └── provider/            # Provider-specific quirks (Gmail, Zoho, etc.)
│   ├── account/                 # Multi-account management
│   ├── filter/                  # Rule engine for auto-forward/labeling
│   ├── translate/               # Google Translate integration
│   ├── ai/                      # OpenRouter integration for compose/reply
│   ├── store/                   # SQLite repository layer
│   │   ├── migrations/          # SQL migrations (embed)
│   │   └── queries/             # SQL queries (embed)
│   └── config/                  # TOML config parsing
├── web/
│   ├── templates/               # Go html/template files
│   │   ├── layouts/
│   │   ├── pages/
│   │   ├── partials/            # htmx swappable fragments
│   │   └── components/          # Reusable template components
│   ├── static/
│   │   ├── css/
│   │   │   └── app.css          # Tailwind input file
│   │   ├── js/
│   │   │   ├── app.js           # Alpine.js components + htmx config
│   │   │   └── sw.js            # Service worker
│   │   ├── icons/               # PWA icons (multiple sizes)
│   │   └── manifest.json
│   └── embed.go                 # go:embed for static + templates
├── migrations/                  # SQL migration files
├── .github/
│   └── workflows/
│       ├── ci.yml               # Lint + test on push/PR
│       └── release.yml          # Build + push Docker image on release
├── Dockerfile                   # Multi-stage build
├── docker-compose.yml           # Dev and prod profiles
├── Makefile                     # All dev/ops commands
├── config.example.toml          # Example configuration
├── README.md
├── LICENSE                      # MIT
├── .air.toml                    # Hot reload config
├── .golangci.yml                # Linter config
├── tailwind.config.js
└── go.mod
```

---

## Features — Full Spec

### 1. Multi-account IMAP sync

- Support N accounts configured in `config.toml`
- Each account: IMAP host/port/user/pass or OAuth2 credentials
- Provider presets for Gmail, Zoho, Purelymail (auto-fill host/port/security)
- IMAP IDLE for push notifications where supported, fallback to polling (configurable interval)
- Incremental sync: use UIDVALIDITY + HIGHESTMODSEQ (CONDSTORE) where available
- Cache message metadata (envelope, flags, headers, threading refs) in SQLite
- Fetch message bodies on-demand (not pre-cached), with LRU cache for recent messages
- Support IMAP SPECIAL-USE for auto-detecting Inbox/Sent/Drafts/Trash/Spam/Archive
- Graceful handling of disconnects, reconnects, and provider rate limits

### 2. SMTP sending

- Per-account SMTP config (host/port/auth)
- Support PLAIN, LOGIN, XOAUTH2 auth mechanisms
- Compose new emails with To/Cc/Bcc, subject, rich text body
- Reply / Reply-All / Forward with proper `In-Reply-To` and `References` headers
- Save sent messages to the provider's Sent folder via IMAP APPEND
- Draft auto-save (local SQLite + IMAP Drafts folder sync)

### 3. Threading

- Implement JWZ threading algorithm (https://www.jwz.org/doc/threading.html)
- Fall back to subject-based grouping when headers are missing
- Gmail: use `X-GM-THRID` when available for perfect threading
- Thread view: collapsed by default, expandable, newest-last

### 4. Folders & Labels

- Full folder tree from IMAP LIST
- Standard folders auto-detected via SPECIAL-USE or name heuristics
- Move messages between folders (IMAP MOVE or COPY+DELETE)
- Gmail labels rendered as tags on messages (multiple labels per message)
- Folder unread counts, updated via IDLE/poll
- Unified inbox view (all accounts merged, sorted by date)

### 5. Star/Favorite

- Toggle star maps to IMAP \Flagged flag
- Visual star icon in message list and detail view
- Filter view: "Starred" across all accounts

### 6. Search (powerful, multi-scope)

Search is a first-class feature, not an afterthought. It should feel as fast and capable as Gmail's search bar but work across all configured accounts.

#### Scopes

Users can search at three levels, switchable via a dropdown or keyboard shortcut:

| Scope | Behavior |
|---|---|
| **Current folder** | Searches only the active folder of the active account |
| **Current account** | Searches all folders in the active account |
| **All accounts** | Searches across every configured account, results grouped by account |

Default scope: current account. The scope selector sits inline in the search bar (e.g., a pill: `[All accounts ▾]`). Keyboard shortcut to cycle scopes while search is focused: `Tab`.

#### Search modes

1. **Local (instant)** — Searches the SQLite metadata cache: From, To, Subject, Date, flags. This powers the initial typeahead results as the user types (debounced at 150ms). Results appear in a dropdown overlay below the search bar, showing top 5–8 matches with highlighted terms.

2. **Server (deep)** — When the user presses Enter or clicks "Search on server", issue IMAP SEARCH commands to the relevant account(s). This finds messages not yet cached locally (e.g., old emails never synced). For Gmail, prefer the `X-GM-RAW` extension which supports Gmail's full query syntax. For standard IMAP servers, compose SEARCH criteria from the parsed query. Server search runs in parallel across accounts when scope is "All accounts", with per-account loading indicators and results streaming in as they arrive (via SSE or htmx polling).

3. **Combined** — Local results show instantly; server results merge in as they arrive, deduplicated by Message-ID. A subtle indicator shows "Searching server..." until all accounts respond or timeout (10s per account).

#### Query syntax

Support a structured query language that degrades gracefully to plain-text search:

| Operator | Example | Maps to |
|---|---|---|
| `from:` | `from:alice@example.com` | IMAP `FROM` / SQLite `from_address` |
| `to:` | `to:bob@work.com` | IMAP `TO` / SQLite `to_addresses` |
| `cc:` | `cc:team@work.com` | IMAP `CC` |
| `subject:` | `subject:quarterly report` | IMAP `SUBJECT` / SQLite `subject` |
| `body:` | `body:deadline` | IMAP `BODY` (server-only, not cached locally) |
| `has:attachment` | `has:attachment` | IMAP `HEADER Content-Type multipart/mixed` or similar heuristic |
| `has:link` | `has:link` | Local heuristic: body contains `http` |
| `is:unread` | `is:unread` | IMAP `UNSEEN` / SQLite `is_read = 0` |
| `is:read` | `is:read` | IMAP `SEEN` / SQLite `is_read = 1` |
| `is:starred` | `is:starred` | IMAP `FLAGGED` / SQLite `is_starred = 1` |
| `in:` | `in:trash`, `in:sent` | Restrict to a specific folder |
| `label:` | `label:work` | Gmail labels (Gmail accounts only) |
| `before:` | `before:2025-01-01` | IMAP `BEFORE` / SQLite `date <` |
| `after:` | `after:2025-06-01` | IMAP `SINCE` / SQLite `date >=` |
| `larger:` | `larger:5mb` | IMAP `LARGER` / SQLite `size >` |
| `smaller:` | `smaller:100kb` | IMAP `SMALLER` / SQLite `size <` |
| `-` (negate) | `-from:noreply@` | IMAP `NOT FROM` / SQLite `from_address NOT LIKE` |
| `""` (exact) | `"board meeting"` | Exact phrase match |
| bare words | `quarterly report` | Match anywhere: subject, from, to (local); IMAP `TEXT` (server) |

Operators are case-insensitive. Multiple operators AND together. Bare words AND together. No explicit OR support (keep it simple).

#### Search UI

- **Search bar**: Centered in the top bar, expands on focus (`/` shortcut). Placeholder: `"Search mail — try from:alice or has:attachment"`.
- **Autocomplete chips**: As the user types an operator (e.g., `from:`), show a dropdown of known values from the local cache (recent senders, folder names, labels). Selecting one inserts it as a visual chip/pill in the search bar.
- **Active filters as pills**: Each parsed operator renders as a removable pill in the search bar (e.g., `[from:alice ✕] [is:unread ✕] quarterly`). Click ✕ to remove a filter. This makes complex queries scannable.
- **Results view**: Replaces the message list. Grouped by account when scope is "All accounts" (collapsible account headers with unread count badge). Each result shows: sender avatar, subject (highlighted match), snippet (highlighted match), date, account badge, folder badge.
- **Empty state**: `"No messages match your search. Try broadening your terms or searching on server."` with a `[Search on server]` button if server search hasn't run yet.
- **Search history**: Last 10 searches stored in SQLite, shown as suggestions when the search bar is focused with no input. Clear history button.
- **Saved searches**: Users can save a query as a "smart folder" that appears in the sidebar under a "Saved Searches" section. Clicking it re-runs the search. Stored in SQLite.

#### Implementation notes

- Parse the query string into a structured `SearchQuery` object in Go (operators + free text)
- Translate `SearchQuery` → SQL WHERE clause for local search
- Translate `SearchQuery` → IMAP SEARCH command(s) for server search
- For "All accounts" server search: fan out goroutines per account, collect results into a channel, stream to frontend via SSE as they arrive
- Index SQLite columns used in search: `from_address`, `subject`, `date`, `is_read`, `is_starred`. Consider FTS5 virtual table for full-text local search over subject + cached body snippets
- Rate-limit server searches to avoid hammering IMAP servers (max 1 concurrent SEARCH per account)
- Cache server search results in SQLite so re-running the same query is instant

### 7. HTML email rendering (privacy-first)

- Render HTML emails inside a sandboxed `<iframe>` with `sandbox="allow-popups"` (no scripts, no same-origin)
- Apply strict CSP: block all external resources by default (images, fonts, CSS, tracking pixels)
- Strip `<script>`, `<object>`, `<embed>`, event handlers, `javascript:` URLs
- Sanitize with an allowlist approach (not blocklist)
- "Load external content" button per-email, with per-sender "always allow" preference
- Plain-text fallback for emails without HTML part
- Dark mode adaptation for HTML emails: inject a `prefers-color-scheme: dark` media query wrapper
- Display attached images inline where referenced by `Content-ID`

### 8. Filter / Rules engine

- Rules defined per-account or globally
- Match conditions: From (contains/regex), To, Subject (contains/regex), Body (contains)
- Actions: auto-forward to address, move to folder, mark as read, star, delete, apply label
- Rules evaluated on new message arrival (via IDLE/poll sync)
- Rules management UI with drag-to-reorder priority
- Test rule against existing messages (dry run)
- Rules stored in SQLite, exportable as JSON

### 9. Auto-translation

- "Translate" button on any message body
- Uses Google Translate API (free tier or API key)
- Auto-detect source language
- Target language configurable in settings (default: English)
- Translated text shown inline below original, clearly marked
- Cache translations in SQLite to avoid re-fetching

### 10. AI compose / reply

- "AI Compose" button in compose view
- "AI Reply" button when viewing a message
- Backend calls OpenRouter API (configurable model, default: a capable but cheap model)
- For replies: sends the email thread context + user's optional instructions
- For compose: user provides topic/intent, AI generates draft
- AI output inserted as editable draft — user always reviews before sending
- OpenRouter API key configured in `config.toml`
- Prompt templates stored as Go templates, customizable

### 11. Authentication

- Single-user by default (this is a self-hosted personal client)
- Setup wizard on first run: create admin user (username + password)
- Password hashed with argon2id
- Session-based auth with secure, httponly, samesite cookies
- Session timeout configurable (default: 30 days)
- CSRF protection on all mutating endpoints
- Optional: TOTP 2FA

### 12. PWA

- `manifest.json` with app name, icons, theme color, display: standalone
- Service worker: cache static assets (CSS, JS, icons), network-first for API calls
- Offline: show cached inbox, queue compose/reply for send when back online
- Install prompt on mobile browsers
- Push notifications for new mail (via SSE from backend → service worker → Notification API)

### 13. Keyboard shortcuts

Global (always active unless a text input is focused):

| Key | Action |
|---|---|
| `j` | Next message / thread |
| `k` | Previous message / thread |
| `o` or `Enter` | Open message / thread |
| `Escape` | Back to list / close modal |
| `r` | Mark as read |
| `u` | Mark as unread |
| `R` | Reply |
| `A` | Reply All |
| `F` | Forward |
| `s` | Toggle star |
| `#` or `d` | Delete (move to Trash) |
| `!` | Report spam (move to Spam) |
| `e` | Archive |
| `c` | Compose new |
| `/` | Focus search |
| `Tab` (in search) | Cycle search scope (folder → account → all) |
| `Escape` (in search) | Clear search and return to inbox |
| `?` | Show keyboard shortcuts help overlay |
| `g i` | Go to Inbox |
| `g s` | Go to Starred |
| `g d` | Go to Drafts |
| `g t` | Go to Sent (Transmitted) |
| `1`–`9` | Switch account (by order in config) |

Implement via Alpine.js `x-on:keydown.window` with a central keybinding manager. Show a `?` floating hint in the bottom-right corner. Shortcuts should be discoverable: on hover/focus of any actionable element, show the shortcut key in a tooltip.

---

## UI/UX — Design System

### Philosophy

SM should look and feel like a premium, modern email client. Think: Linear meets Superhuman meets Arc — clean, fast, opinionated. Not a 2005 webmail clone. Every interaction should feel instant. Every screen should be beautiful enough to screenshot.

### Design tokens

- **Dark mode by default**. Light mode toggle available. System preference respected.
- **Color palette**: Zinc-based neutrals. Single accent color: indigo-500 for primary actions, amber-400 for stars, red-500 for destructive. Unread messages: slightly brighter text (zinc-100 vs zinc-400 for read).
- **Typography**: Inter for UI, JetBrains Mono for code/monospace. Tight leading, generous letter-spacing on headings.
- **Spacing**: 4px grid. Compact density by default (email clients need to show a lot of data).
- **Radius**: rounded-lg (8px) for cards/modals, rounded-md (6px) for buttons/inputs, rounded-full for avatars/badges.
- **Shadows**: Subtle, layered. `shadow-sm` for cards, `shadow-lg` for modals/dropdowns.
- **Transitions**: 150ms ease for hovers, 200ms for modals/panels.

### Layout

```
┌──────────────────────────────────────────────────────┐
│ Top bar: Logo · Search (center) · Settings · Avatar  │
├──────────┬───────────────┬───────────────────────────┤
│ Sidebar  │ Message List   │ Reading Pane (optional)   │
│          │               │                           │
│ Accounts │ Sender avatar │ From/To/Date header       │
│ ├─ Gmail │ Subject bold  │ Thread messages            │
│ ├─ Zoho  │ Preview gray  │ HTML body (sandboxed)      │
│ ├─ Pure  │ Time · Star   │                           │
│          │ Unread dot     │ Reply bar at bottom       │
│ Folders  │               │                           │
│ ├─ Inbox │               │                           │
│ ├─ Sent  │               │                           │
│ ├─ ...   │               │                           │
│          │               │                           │
│ Labels   │               │                           │
│ Storage  │               │                           │
├──────────┴───────────────┴───────────────────────────┤
│ Status bar: Sync status · Shortcut hint (?)          │
└──────────────────────────────────────────────────────┘
```

- **Three-column layout** on desktop (sidebar collapsible)
- **Two-column** on tablet (list + reading pane, sidebar as overlay)
- **Single-column** on mobile (list → detail navigation, bottom tab bar)
- Reading pane position: right (default) or bottom (configurable)

### States & polish

Every view must handle these states:

1. **Loading**: Skeleton loaders that mirror the exact layout of the content they replace. No spinners. Skeletons should pulse subtly (Tailwind `animate-pulse` on zinc-800 shapes).

2. **Empty state**: Illustrated + copy for every empty view:
   - Inbox zero: `"You're all caught up. Go touch some grass. 🌱"`
   - No search results: `"Nothing here. Try a different search or blame IMAP."`
   - No starred: `"No favorites yet. Star something you'll definitely read later (but won't)."`
   - No drafts: `"Clean slate. Channel that creative energy."`
   - No spam: `"Spam folder is empty. The filters are earning their keep."`

3. **Error state**: Friendly, actionable. `"Couldn't sync Gmail. Check your OAuth token or try again." [Retry] [Settings]`

4. **Optimistic UI**: Star toggling, mark-as-read, move-to-trash — reflect immediately in the UI, sync to IMAP in background. Roll back on failure with a toast.

5. **Toasts**: Bottom-right. Auto-dismiss after 5s. Actions where applicable ("Undo" for delete/archive within a 10s window). Stack up to 3.

6. **Tooltips**: On all icon buttons, showing label + keyboard shortcut.

7. **Transitions**: Message list items slide out on delete/archive. Reading pane content crossfades on message switch. Sidebar folds/unfolds with smooth width transition.

### Logo

Simple, geometric, memorable. The letters "SM" stylized — consider:
- A minimal envelope shape where the flap forms an "S" and the body forms an "M"
- Or just clean sans-serif "SM" in a rounded square, indigo-on-zinc
- SVG, works at 16px (favicon) through 512px (PWA splash)
- Generate multiple sizes for PWA manifest

### Micro-interactions

- Hover on message row: subtle background shift (zinc-800 → zinc-750)
- Click star: star icon fills with a quick scale bounce (100ms scale to 1.2, back to 1)
- Unread count badge: slides in/out with a number transition
- Compose button: floating action button on mobile, subtle pulse on first visit as a hint
- Pull-to-refresh on mobile (via htmx trigger)
- Sender avatars: generated from initials with deterministic colors based on email hash (no Gravatar by default — privacy)

---

## Configuration

`config.toml` example:

```toml
[server]
host = "0.0.0.0"
port = 8080
base_url = "https://mail.example.com"
secret = "change-me-to-a-random-64-char-string"

[db]
path = "./data/sm.db"

[[accounts]]
name = "Personal"
provider = "gmail"  # preset: fills imap/smtp host+port
email = "me@gmail.com"
auth = "oauth2"
oauth2_client_id = "..."
oauth2_client_secret = "..."
# oauth2 tokens stored in DB after initial auth flow

[[accounts]]
name = "Work"
provider = "zoho"
email = "me@zoho.com"
auth = "oauth2"
oauth2_client_id = "..."
oauth2_client_secret = "..."

[[accounts]]
name = "Privacy"
provider = "purelymail"
email = "me@mydomain.com"
auth = "password"
password = "app-password-here"
imap_host = "imap.purelymail.com"
imap_port = 993
smtp_host = "smtp.purelymail.com"
smtp_port = 465

[translate]
api_key = ""  # Google Translate. Empty = disabled.
target_language = "en"

[ai]
openrouter_api_key = ""  # Empty = disabled.
model = "anthropic/claude-sonnet-4-6"

[sync]
poll_interval = "5m"  # Fallback when IDLE not supported
max_messages_per_sync = 500
```

---

## Docker

### Dockerfile (multi-stage)

```dockerfile
# Stage 1: Build Tailwind CSS
FROM node:22-alpine AS css
WORKDIR /build
COPY package.json tailwind.config.js ./
COPY web/static/css/ web/static/css/
COPY web/templates/ web/templates/
RUN npm ci && npx @tailwindcss/cli -i web/static/css/app.css -o web/static/css/dist.css --minify

# Stage 2: Build Go binary
FROM golang:1.23-alpine AS go
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=css /build/web/static/css/dist.css web/static/css/dist.css
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o sm ./cmd/sm

# Stage 3: Final image
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=go /build/sm /usr/local/bin/sm
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["sm"]
```

### docker-compose.yml

```yaml
services:
  sm:
    image: ghcr.io/mattmezza/sm:latest
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data
      - ./config.toml:/etc/sm/config.toml:ro
    environment:
      - SM_CONFIG=/etc/sm/config.toml
      - TZ=Europe/Zurich
    restart: unless-stopped
```

---

## CI/CD — GitHub Actions

### `.github/workflows/ci.yml`

Runs on every push and PR:
- `golangci-lint` (using `.golangci.yml` config)
- `go test ./...` with race detector
- `go vet ./...`
- Tailwind CSS build (verify it compiles)
- **No Docker image build** — CI only validates code quality and tests

### `.github/workflows/release.yml`

Runs **only** on GitHub Release creation (triggered by `make release`):
- Waits for CI to pass on the tagged commit
- Multi-platform Docker build (linux/amd64, linux/arm64)
- Push to `ghcr.io/mattmezza/sm:<tag>` and `ghcr.io/mattmezza/sm:latest`
- Attach the Go binary (linux/amd64, linux/arm64, darwin/arm64) to the GitHub Release

Trigger: `on: release: types: [published]`

---

## Makefile

Every command a developer needs. `make help` prints a formatted table of all targets.

```makefile
.DEFAULT_GOAL := help

VERSION ?= dev
BINARY  := sm
IMAGE   := ghcr.io/mattmezza/sm

##@ Development
.PHONY: dev
dev: ## Run with hot reload (requires air)
	air

.PHONY: run
run: ## Build and run locally
	go run ./cmd/sm

.PHONY: css
css: ## Build Tailwind CSS (watch mode)
	npx @tailwindcss/cli -i web/static/css/app.css -o web/static/css/dist.css --watch

.PHONY: css-build
css-build: ## Build Tailwind CSS (production)
	npx @tailwindcss/cli -i web/static/css/app.css -o web/static/css/dist.css --minify

##@ Testing
.PHONY: test
test: ## Run tests
	go test -race -cover ./...

.PHONY: lint
lint: ## Run linter
	golangci-lint run

.PHONY: check
check: lint test ## Run all checks

##@ Build
.PHONY: build
build: css-build ## Build binary
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/sm

.PHONY: docker
docker: ## Build Docker image locally
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

##@ Release
.PHONY: release
release: ## Create a GitHub release (usage: make release name=v0.1)
	@if [ -z "$(name)" ]; then echo "Usage: make release name=vX.Y"; exit 1; fi
	@echo "Creating release $(name)..."
	git tag -a $(name) -m "Release $(name)"
	git push origin $(name)
	gh release create $(name) --generate-notes --title "$(name)"
	@echo "Release $(name) created. GitHub Actions will build and push the Docker image."

##@ Setup
.PHONY: setup
setup: ## Install development dependencies
	go install github.com/air-verse/air@latest
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	npm install
	cp -n config.example.toml config.toml 2>/dev/null || true
	@echo "Done. Edit config.toml and run 'make dev'."

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\n\033[1mSM — Simple Mail\033[0m\n\nUsage: make \033[36m<target>\033[0m\n"} \
		/^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)} \
		/^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
```

---

## Versioning

- Semantic versioning with **two digits only**: `vX.Y` (no patch version)
- Major bump (`v1.0` → `v2.0`): breaking config changes, DB migrations that aren't auto-applied
- Minor bump (`v1.0` → `v1.1`): new features, bug fixes, non-breaking changes
- Tags are annotated git tags, created via `make release name=vX.Y`
- The Go binary embeds the version string via `-ldflags`

---

## Subagent Delegation

When delegating subtasks to coding subagents, use these model assignments:

| Task type | Model | Rationale |
|---|---|---|
| Architecture decisions, complex IMAP/threading logic, security-sensitive code (auth, sanitization, CSP), API design, system integration | **Opus** | Needs deep reasoning, security awareness, protocol knowledge |
| Feature implementation (routes, templates, Alpine components, filter engine, API integrations), tests, CI/CD workflows, Dockerfile | **Sonnet** | Bulk of the coding work, good balance of quality and speed |
| Repetitive/mechanical tasks (generating icon sizes, writing migration boilerplate, formatting help text, creating skeleton loaders, stub files) | **Haiku** | Fast, cheap, good enough for templated work |

When spawning a subagent:
- Always pass it the relevant section of this spec as context
- Always specify which files it should create/modify
- Always specify the acceptance criteria (what "done" looks like)
- Tell it to write tests for any non-trivial logic
- Tell it to follow the existing code style and patterns

---

## Implementation Phases

### Phase 1 — Foundation (Opus + Sonnet)
Skeleton project, build system, CI, Docker, config parsing, SQLite setup with migrations, auth (signup/login/session), basic page layout with sidebar + empty states. Tailwind + htmx + Alpine wired up. PWA manifest + service worker shell. Logo. `make help` works. `make dev` hot-reloads. CI passes on push.

**Done when**: You can `docker compose up`, hit localhost:8080, see a login screen, log in, and see an empty inbox with the full layout chrome (sidebar, top bar, status bar), all in dark mode.

### Phase 2 — IMAP Core (Opus + Sonnet)
Connect to one IMAP account (start with Purelymail — most standards-compliant). List folders. Fetch message list with envelopes. Cache in SQLite. Render message list with skeleton loading. Open a message, fetch body, render sanitized HTML in sandboxed iframe. Star toggle. Mark read/unread. Delete. Folder navigation.

**Done when**: You can see your real inbox, read emails, star them, delete them, and navigate folders. All with keyboard shortcuts working.

### Phase 3 — SMTP + Compose (Sonnet)
Compose new email (To/Cc/Bcc, subject, body editor). Send via SMTP. Reply/Reply-All/Forward with correct headers. Draft auto-save. Sent folder sync.

**Done when**: You can compose, reply, forward, and find your sent messages in the Sent folder on the actual provider.

### Phase 4 — Threading + Multi-account (Opus + Sonnet)
JWZ threading. Thread view UI. Add Gmail account (OAuth2 flow) and Zoho account. Unified inbox. Account switcher. Provider-specific quirks (Gmail labels, XOAUTH2).

**Done when**: All three accounts show up, threads render correctly, Gmail labels display, and you can switch between accounts or view unified inbox.

### Phase 5 — Search + Filters (Opus for query parser + IMAP SEARCH fan-out, Sonnet for UI + filters)
Query parser (operators → structured SearchQuery). SQLite FTS5 index for instant local search. IMAP SEARCH fan-out with per-account goroutines + SSE streaming. Search UI: autocomplete chips, operator pills, scope selector, search history, saved searches as smart folders. Filter/rules engine: create, edit, delete, reorder rules. Rules execute on new mail. Rule test dry run.

**Done when**: You can type `from:boss@company.com has:attachment after:2025-01-01` in the search bar scoped to "All accounts", see instant local results plus server results streaming in grouped by account. You can save this as a smart folder. You can create a rule "forward emails from boss@company.com to me@personal.com", and it works on new arrivals.

### Phase 6 — AI + Translation (Sonnet + Haiku)
Google Translate integration. OpenRouter AI compose + AI reply. UI for both: buttons, inline results, editable output.

**Done when**: You can translate a German email to English, and ask AI to draft a reply.

### Phase 7 — Polish (Sonnet + Haiku)
All micro-interactions. Toasts with undo. Optimistic UI everywhere. Full keyboard shortcut coverage. PWA offline support. Push notifications via SSE. Mobile responsive polish. Accessibility audit (ARIA labels, focus management, screen reader testing). Performance audit (Core Web Vitals). README with screenshots.

**Done when**: You'd be proud to show this to a designer friend.

### Phase 8 — Release v0.1 (Sonnet)
Final test pass. `make release name=v0.1`. Docker image on ghcr.io. README with setup instructions. `config.example.toml` documented. One-command setup: `docker compose up`.

---

## Non-Negotiables

1. **No external tracking or analytics.** This is a privacy tool.
2. **No CDN-hosted assets.** Everything bundled. The PWA works on a private network.
3. **All secrets in config file or env vars.** Never hardcoded, never logged.
4. **Every IMAP operation must handle errors gracefully.** Disconnects, timeouts, provider-specific errors — never crash, always surface to the user.
5. **HTML email sanitization is security-critical.** Treat it like input validation on a login form. Allowlist approach only. Test with adversarial emails.
6. **The UI must be fast.** Target: inbox loads in <200ms (cached), message opens in <100ms (cached), <1s (IMAP fetch). Use htmx's `hx-indicator` for anything slower.
7. **Mobile is not an afterthought.** Test every feature at 375px width.
8. **Copy should be human.** Not corporate, not cutesy. Friendly, direct, occasionally funny. Like a tool built by someone who actually uses email.
