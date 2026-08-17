# Licensing

mimux is a monorepo containing two separately licensed bodies of code. GitHub
displays a single licence badge derived from the root `LICENSE` file, so that
badge reads **AGPL-3.0** — but it does not describe the whole tree. This file
does.

Copyright on every part of this repository is held by **Matteo Merola**. That
single fact is what makes the arrangement below work; see
[Why this is not a licence violation](#why-this-is-not-a-licence-violation).

## What is under which licence

| Path | Licence | What it is |
|------|---------|------------|
| `cmd/`, `internal/`, `web/` | **AGPL-3.0-only** | The mimux mail client. The whole thing. |
| `pro/` | **Elastic Licence 2.0** (`pro/LICENSE`) | The commercial automation layer: REST API, MCP server, webhooks, AI compose. |
| `www/` | **AGPL-3.0-only** | Landing page and docs for mimux.dev. |
| Everything else at the root | **AGPL-3.0-only** | Build files, CI, docs. |

Every Go file carries an `SPDX-License-Identifier` header, so the split is
machine-checkable:

```sh
grep -rL 'SPDX-License-Identifier' --include='*.go' .   # should print nothing
grep -rl 'LicenseRef-Elastic-2.0' --include='*.go' .    # should print only pro/
```

## The free/pro line

This line was published before `pro/` existed, and it does not move.

**Free, AGPL-3.0, forever — the complete mail client.** Multi-account unified
inbox, IMAP IDLE sync, threading, search (local and server-side), compose with
rich text and Markdown, drafts, undo-send, scheduled send, filters and rules,
calendar invites, keyboard shortcuts, PWA install, Web Push and ntfy
notifications, backup and restore. If a human uses mimux to read and write mail,
it is free.

**Paid, ELv2 — automation only.** The REST API, the MCP server, webhooks, and AI
compose. If something *other than a human* drives mimux, that is the paid layer.

Nothing is ever moved from the free side to the paid side. The client does not
become a demo, features are not held back to seed a future tier, and no part of
mail sync, sending, or storage will ever live behind a licence key.

## Building each half

The separation is enforced by Go build tags, not by convention. Every file in
`pro/` begins with `//go:build pro`, including the registration hook that binds
it into the server.

```sh
make build       # free binary — contains zero ELv2 code
make build-pro   # commercial binary — AGPL client + ELv2 pro layer
make verify-free # proves the free binary links nothing from pro/
```

`make build` does not pass the `pro` tag, so the Go toolchain excludes every
file in `pro/` from the build graph entirely — not compiled, not linked, not
present in the binary. You do not have to take that on trust: read the
`Makefile`, or run `make verify-free`, which greps the compiled package list for
`pro` and fails if it appears.

## Why this is not a licence violation

The pro binary combines AGPL-3.0 client code with ELv2 pro code and ships the
result under ELv2. If two different parties held the copyrights, that would be a
problem — the AGPL requires derivative works to be distributed under the AGPL,
and ELv2 is not AGPL-compatible.

They are not two parties. **Matteo Merola holds the copyright on both halves.**

A copyright holder is not bound by the licence they grant to others. The AGPL is
the licence *I offer you* for the client; it is not a constraint on what I may do
with code I own. I am free to licence my own work under as many licences as I
like, including combining it with other work of mine under different terms. This
is the same mechanism behind every dual-licensed project — Qt, MySQL, and
Sidekiq all work this way.

Concretely:

- **You** receive the client under AGPL-3.0. If you modify it and run it as a
  network service, you must publish your modifications. That obligation is real
  and I intend to enforce it.
- **You** receive `pro/` under ELv2, if you have bought a licence. You may not
  resell it as a hosted service or circumvent the licence key.
- **I** grant myself a commercial exception to the AGPL on the client code, which
  I can do because I own it, and distribute the combined pro binary under ELv2.

This is why [`CLA.md`](CLA.md) exists and why it is not optional. A contribution
to the AGPL client from someone else would be *their* copyright, and I could not
include it in the pro binary without their permission. The CLA obtains that
permission up front, so contributions can be accepted without creating a licence
trap that only becomes visible years later.

## Commercial exceptions for third parties

If you want to embed mimux in a product without opening your own source, the AGPL
is not your only option — I can sell you a commercial licence, for the same
reason described above. Email <mattmezza@gmail.com>.

## Trademark

`mimux` is a trademark of Matteo Merola. The licences above cover the *code*;
they grant no rights to the name or the logo.

You may say your product works with mimux, or that it is a fork of mimux. You may
not name your fork or your service `mimux`, or anything close enough to suggest
it is the official one. If you distribute a modified version, rename it — this
is the ordinary courtesy of open source and it is also what keeps the name
meaning something.

## Questions

If any of the above is unclear, open an issue rather than assuming the worst
reading. Ambiguity here is a bug in this file and I would like to fix it.
