# Contributor Licence Agreement

**Read this before opening a pull request.** It is short and it matters.

By submitting a contribution to mimux, you agree to the terms below. There is no
form to sign and nothing to email: opening a pull request is your acceptance, and
the maintainer will ask you to confirm it in the PR if there is any doubt.

## Why this exists

mimux is dual-licensed. The client is AGPL-3.0; the `pro/` automation layer is
ELv2 and is sold. That arrangement only works because one party — Matteo Merola —
holds the copyright on both halves. See [LICENSING.md](LICENSING.md) for the full
explanation.

If you contribute code and retain sole copyright over it, that code cannot be
included in the commercial build, because AGPL code owned by someone else cannot
be relicensed by me. In a monorepo this problem is invisible until the day it
isn't: a helper you wrote in `internal/mail` gets called from `pro/`, and
suddenly the pro binary cannot legally ship.

This document prevents that, up front, once, instead of discovering it later and
having to rip out or rewrite work that was gratefully accepted.

## The terms

**1. You keep your copyright.** You are not assigning or transferring ownership.
Your contribution remains yours.

**2. You grant a licence to the maintainer.** You grant Matteo Merola a
perpetual, worldwide, non-exclusive, royalty-free, irrevocable licence to
reproduce, modify, distribute, sublicense, and otherwise use your contribution,
**including under licence terms other than the AGPL** — specifically including
the Elastic Licence 2.0 used by `pro/`, and including any future commercial
licence or commercial exception granted to a third party.

**3. You grant the same to everyone else, under the AGPL.** Your contribution to
the client is also licensed to the public under AGPL-3.0, exactly like the rest
of the client. You are not giving the maintainer something the community does not
get.

**4. You grant a patent licence.** You grant a perpetual, worldwide,
non-exclusive, royalty-free, irrevocable patent licence covering any patent
claims you own or control that your contribution would otherwise infringe.

**5. You confirm you have the right to do this.** You wrote the contribution
yourself, or you otherwise have the necessary rights to submit it under these
terms. In particular: if your employment contract assigns your work to your
employer, you have their permission. If your contribution includes third-party
code, you have said so in the pull request and identified its licence.

**6. No warranty.** Your contribution is provided as is, without warranty of any
kind. You are not taking on support obligations by contributing.

**7. No obligation to merge.** Submitting a contribution does not obligate the
maintainer to accept, merge, or ship it.

## What counts as a contribution

Any code, documentation, configuration, translation, test, or other original work
you deliberately submit for inclusion in this repository — via pull request,
patch, issue attachment, or any other means.

Bug reports, feature requests, and discussion are **not** contributions in this
sense. You can file issues freely without agreeing to anything.

## What is not accepted

Pull requests touching `pro/` are not accepted from outside contributors. That
directory is the commercial product; changes to it need to come from the
copyright holder for the licensing to stay clean. Please open an issue instead —
ideas and bug reports about the pro layer are very welcome, code is not.

## Fine print

- If you have contributed before this file existed, thank you, and please
  confirm your agreement on your next PR or in an issue.
- Nothing here grants you rights to the `mimux` trademark. See
  [LICENSING.md](LICENSING.md).
- This is a plain-language CLA modelled on the Apache ICLA. If you need your
  employer's legal team to review something with more ceremony, email
  <legal@mimux.dev> and we will sort it out.
