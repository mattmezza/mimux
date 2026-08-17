<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Inbox triage with Claude — the mimux MCP demo

A single-file agent that connects to a running **mimux pro** instance over the
MCP streamable-HTTP endpoint (`/api/mcp`) and triages the inbox: it reads the
unread mail, stars what needs a human, archives newsletters and notifications,
and drafts — never sends — replies where one is obvious.

This is also the script for the launch demo video: everything it does is
visible live in the mimux UI while it runs (stars appear, threads leave the
inbox, drafts show up in Drafts).

## Safety model

Nothing here can send mail. mimux's MCP surface deliberately splits outbound
mail into `draft_reply` (creates a normal mimux draft and returns a preview)
and `send_draft` (sends it). This script strips `send_draft` from the tool
list before the model ever sees it, and the system prompt says drafts wait
for the human. You review the drafts in mimux and press Send yourself — or
delete them.

Also: moving mail to trash requires `confirm=true` at the tool level, and the
prompt forbids trash/spam moves entirely. The worst realistic outcome is an
over-eager archive, which is one drag-and-drop to undo.

## Setup

1. Run a **pro** build of mimux and create an API token in **Settings → API**
   with the `mail:read`, `mail:send` and `mail:modify` scopes.
2. Get an Anthropic API key.
3. Install [uv](https://docs.astral.sh/uv/) (the script declares its own
   dependencies inline — no venv or requirements file).

```sh
export MIMUX_URL=http://localhost:8083   # default; set if mimux runs elsewhere
export MIMUX_TOKEN=mimux_pat_...
export ANTHROPIC_API_KEY=sk-ant-...
uv run triage.py
```

The agent loop runs locally: this script talks to your mimux directly, so a
localhost instance works — your mail never goes anywhere except to the model
as tool results.

## Notes

- Uses the current recommended model (`claude-opus-5`); edit the `MODEL`
  constant to try others.
- Stateless streamable HTTP: every request carries the bearer token; there is
  no session to expire.
- The stdio alternative for MCP clients that can't speak streamable HTTP is
  the bundled bridge: `mimux mcp` (see the API docs), with `MIMUX_URL` and
  `MIMUX_TOKEN` in its environment.
