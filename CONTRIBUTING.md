# Contributing

Read this before you spend an evening on a pull request. It is short, and it will
save you time — mostly by telling you in advance which changes I'm going to say
no to.

Bug reports and good questions are always welcome. Code is welcome too, with the
caveats below.

## Support expectations

mimux is a nights-and-weekends project. I have a day job.

- **Free tier:** GitHub issues and Discussions, community help, best effort from
  me. No guaranteed response time. I read everything; I don't reply to everything.
- **Paid pro licence holders:** email support. That is the difference between the
  tiers — the mail client itself is complete and free forever (see
  [LICENSING.md](LICENSING.md)).

## Before you open an issue

Run this and paste the output:

```sh
make diagnose
```

It is sanitised — secrets, passwords, tokens and keys are redacted. It is the
single most useful thing you can put in a bug report, and it usually answers the
first three questions I would otherwise have to ask.

## Bug reports

Include:

- The mimux version
- How you run it — Docker Compose, Docker by hand, a release binary, or built from
  source
- Which mail provider or IMAP server (Gmail, Fastmail, Dovecot, Zoho, Migadu, …)
- The `make diagnose` output
- Steps to reproduce, and what you expected instead

Provider-specific IMAP behaviour is the single largest source of bugs here, which
is why the provider question is not optional.

## Pull requests

- **Open an issue first** for anything non-trivial. A PR that arrives without a
  prior conversation is a PR I may have to reject on direction alone, and that is
  a waste of your evening.
- **Small, focused diffs.** One change per PR.
- **`make check` must pass** — lint, tests, and the two licence checks. Run it
  before pushing.
- **Match the existing code style.** Prefer the pattern already in the file over
  a better pattern imported from elsewhere.
- **Tests for non-trivial logic.**
- **Dependencies are kept deliberately minimal** and the standard library is
  preferred. A PR that adds a dependency needs to justify why the stdlib won't do.

## Pull requests touching `pro/`

**Not accepted from outside contributors.** `pro/` is the commercial product, and
the dual-licensing arrangement only holds because the copyright on both halves
stays with one party.

Issues, bug reports and ideas about the pro layer *are* very welcome — just not
code. See [LICENSING.md](LICENSING.md) for why, and [CLA.md](CLA.md).

## The CLA

By opening a pull request you agree to the [Contributor Licence
Agreement](CLA.md). Please read it; it is one page.

## Dev setup

```sh
make setup   # air, golangci-lint, npm deps
make css     # tailwind watcher, in a second terminal
make dev     # hot-reloading server
make help    # everything else
make check   # lint + tests, before you push
```

## Things I will probably say no to

Not hostility — just a map of the walls, so you don't build against one.

- **Multi-user or multi-tenancy.** Single-user is a deliberate architectural
  choice that simplifies auth, sync, storage and the whole UI. It is not a missing
  feature and it is not on the roadmap.
- **A JavaScript framework rewrite.** htmx plus Alpine.js with no JS build step is
  the point of the project, not a stage it hasn't outgrown yet.
- **A new runtime dependency for something the standard library already does.**
- **Features that only make sense for a hosted service** — billing, sign-up flows,
  admin dashboards over other people's mailboxes. mimux is a thing you run for
  yourself.
