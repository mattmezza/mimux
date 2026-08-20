---
name: mimux-cli
description: Drive a running mimux mail server from the shell via the `mimux mail <verb>` CLI — list, search, read, triage (mark-read, star, move/archive/trash), and draft/send email. Use whenever the task is to read, search, organize, triage, or send email through mimux, or a `mimux mail ...` / `mimux mcp` / `mimux licence` command needs to be run.
---

# mimux mail CLI

`mimux mail <verb>` is a thin HTTP client of a *running* mimux (default
`http://localhost:8083`) — it never touches the database directly. It mirrors
the mimux MCP tools one for one, for shells and agents that have a bash
harness but no MCP client. Every command takes `--url`, `--token`, `--json`
(prints the API response verbatim, for `jq`), and `-h`. Exit codes: `0` ok,
`1` failed, `2` bad usage.

## Auth — check before doing anything else

```sh
mimux mail whoami
```

- Prints the token's label and scopes if signed in.
- If it says "not signed in", **ask the human** to run
  `mimux mail login <url>` (opens their browser to approve scopes — this is
  a human-in-the-loop step, do not attempt it yourself) or to export
  `MIMUX_TOKEN`/`MIMUX_API_TOKEN` (a token from Settings → API). Do not guess
  or fabricate a token.
- `mimux mail use [<url>]` lists signed-in instances or pins the default when
  more than one is signed in. `mimux mail logout <url>` forgets it locally
  (the token itself still needs revoking in Settings → API).
- Config precedence: `--url` > `MIMUX_URL` > pinned `use` default > only
  signed-in instance > `http://localhost:8083`. Token: `--token` >
  `MIMUX_TOKEN`/`MIMUX_API_TOKEN` > the stored credential for that instance.
  `mimux mcp` (the stdio MCP bridge) resolves both the same way, so a
  `mimux mail login` is enough for it too — no env block required.

## Core workflows (verified against a live instance)

```sh
mimux mail accounts                       # accounts, sync state, unread counts
mimux mail folders [-account NAME]        # folder tree with ids
mimux mail list [-unread] [-starred] [-account NAME] [-folder ID] [-limit N] [-cursor C]
mimux mail search '<query>' [-account NAME] [-folder ID] [-limit N] [-deep] [-wait 30s]
mimux mail read <id> [-html]
mimux mail mark-read <id> [-unread]
mimux mail star <id> [-off]
mimux mail move <id> <folder-id|archive|spam|trash> [-yes]   # -yes required for trash
```

Query language (same as the web search box): bare words match anywhere;
`from:`, `to:`, `subject:`, `body:`, `is:unread`, `is:starred`,
`has:attachment`, `before:YYYY-MM-DD`, `after:YYYY-MM-DD`, quoted phrases,
and a leading `-` negates a term. A query that itself starts with `-` needs
`--` first: `mimux mail search -- -from:noreply is:unread`. `-deep` asks the
IMAP servers instead of the local index (slower, for old/rarely-synced mail).

`--json` on any read command gives machine-parseable output, e.g.:

```sh
mimux mail search 'is:unread from:google' -limit 3 --json | jq -r '.data[] | "\(.id)\t\(.subject)"'
```

### Drafting and sending — never send without explicit human approval

```sh
echo "Thanks — next week works." | mimux mail draft -in-reply-to <id>
```

`draft` only ever saves to Drafts — nothing is sent, ever. Prefer it, or
`mimux mail send ... -dry-run` (prints exactly what would go out, sends
nothing), to show the human what would be sent. **Only run `mimux mail send`
without `-dry-run` after the human has explicitly reviewed and approved the
exact recipients, subject and body.** Do not send on your own judgment, and
do not chain a draft straight into a send.

`-to`/`-cc`/`-bcc` are comma-separated; body comes from `-body` or stdin.
`-in-reply-to <id>` derives recipients/subject/threading from the original
(`-reply-all` keeps every original recipient minus the sender). `-at
<RFC3339>` schedules instead of sending immediately. Sending a saved draft
is not a CLI operation — compose with `send -dry-run` for a preview instead.

## Scopes (from the token's `whoami`)

| Verb(s) | Scope | Note |
|---|---|---|
| `login`, `logout`, `use`, `whoami` | — | no scope needed |
| `accounts` | `accounts:read` | |
| `folders`, `list`, `search`, `read` | `mail:read` | |
| `mark-read`, `star`, `move` | `mail:modify` | |
| `draft`, `send` | `mail:send` | |

A 403 names the missing scope in its message — relay that to the human
rather than retrying; they tick it in Settings → API and mint a new token.

## Failure modes

| Symptom | Cause | Fix |
|---|---|---|
| `not signed in — run mimux mail login ...` | no token found | ask the human to run `login`, or set `MIMUX_TOKEN` |
| `the token was rejected (unknown, revoked or expired)` (401) | bad/expired token | re-run `login`, or mint a new token in Settings → API |
| `this token is missing the <scope> scope` (403) | token lacks the needed scope | tick it in Settings → API, mint a new token |
| `...the pro licence... (402)` | licence expired/absent (mail itself still works) | run `mimux licence status` on the server; keys at account.mimux.dev |
| `couldn't reach <url>: ... (is mimux running?)` | wrong `--url`/`MIMUX_URL`, or server down | confirm the URL and that the mimux process is up |

## Utilities

```sh
mimux -version
mimux upgrade [--check] [--version vX.Y.Z]
mimux completion bash|zsh
mimux licence status        # on the server: licence/trial state, whether the API answers
```
