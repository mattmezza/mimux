# Contributing

The short version: **issues yes, pull requests usually no.**

Bug reports, questions, suggestions and "have you considered X" are genuinely
welcome and are the most useful thing you can send me. Unsolicited code mostly
is not — not because it isn't appreciated, but because reviewing, integrating
and then maintaining someone else's patch forever costs me more evenings than
writing it myself. Read the [Pull requests](#pull-requests) section before you
spend one.

## Support expectations

mimux is a nights-and-weekends project. I have a day job.

- **Free tier:** GitHub issues, best effort from me. No guaranteed response
  time. I read everything; I don't reply to everything.
- **Paid pro licence holders:** email support. That is the difference between
  the tiers — the mail client itself is complete and free forever (see
  [LICENSING.md](LICENSING.md)).

## Before you open an issue

Run this and paste the output:

```sh
make diagnose
```

It is sanitised — secrets, passwords, tokens and keys are redacted, never
printed. It is the single most useful thing you can put in a bug report, and it
usually answers the first three questions I would otherwise have to ask.

## Bug reports

Include:

- The mimux version
- How you run it — Docker Compose, Docker by hand, a release binary, or built
  from source
- Which mail provider or IMAP server (Gmail, Fastmail, Dovecot, Zoho, Migadu, …)
- The `make diagnose` output
- Steps to reproduce, and what you expected instead

Provider-specific IMAP behaviour is the single largest source of bugs here,
which is why the provider question is not optional.

## Suggestions, questions, ideas

Open an issue. Blank issues are enabled precisely so that things which don't fit
a template have somewhere to go. There is no Discussions tab.

A good suggestion describes the situation you hit, not the feature you imagined
— the concrete annoyance is the part I can actually work with.

## Pull requests

**I do not generally accept pull requests.** Please open an issue instead and
let me implement it, or let me tell you why I won't.

The exceptions, where a PR is genuinely welcome:

- A **one-line or few-line fix** for a bug already discussed in an issue —
  typos, a wrong constant, an off-by-one, a broken link.
- Something **I explicitly asked you for** in an issue thread.

If it's neither of those, an issue will get further than a PR, and faster.

For the rare PR that is in scope:

- `make check` must pass — lint, tests, and the two licence checks.
- Match the existing code style. Prefer the pattern already in the file over a
  better pattern imported from elsewhere.
- Dependencies are kept deliberately minimal and the standard library is
  preferred.
- By opening a pull request you agree to the [Contributor Licence
  Agreement](CLA.md). It is one page and it exists because mimux is
  dual-licensed — see [LICENSING.md](LICENSING.md).

**PRs touching `pro/` are never accepted.** That directory is the commercial
product, and the dual-licensing arrangement only holds because copyright on both
halves stays with one party. Issues and ideas about the pro layer are welcome;
code is not.

## Forking

The client is AGPL-3.0 and forking is a legitimate thing to do — if you want it
to go somewhere I don't, that is what the licence is for. Two asks: publish your
changes if you run it as a network service (the AGPL requires this), and rename
it, because `mimux` is a trademark. See [LICENSING.md](LICENSING.md).

## Dev setup

```sh
make setup   # air, golangci-lint, npm deps
make css     # tailwind watcher, in a second terminal
make dev     # hot-reloading server
make help    # everything else
make check   # lint + tests + licence checks
```

## Things I will probably say no to

Not hostility — just a map of the walls, so you don't build against one.

- **Multi-user or multi-tenancy.** Single-user is a deliberate architectural
  choice that simplifies auth, sync, storage and the whole UI. It is not a
  missing feature and it is not on the roadmap.
- **A JavaScript framework rewrite.** htmx plus Alpine.js with no JS build step
  is the point of the project, not a stage it hasn't outgrown yet.
- **A new runtime dependency for something the standard library already does.**
- **Features that only make sense for a hosted service** — billing, sign-up
  flows, admin dashboards over other people's mailboxes. mimux is a thing you
  run for yourself.
