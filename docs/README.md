# docs/ — the API reference site

The static site published at **<https://docs.mimux.dev>**: an OpenAPI reference
for the mimux REST API.

This directory is AGPL-3.0 like the rest of the repo root. The *spec it renders*
lives at [`pro/openapi.json`](../pro/openapi.json) and is ELv2, because it
describes the pro layer. Nothing here is compiled into any binary.

## Layout

| Path | What it is |
|---|---|
| `src/index.html` | The page shell — mimux.dev's "correspondence" header and palette around Scalar. Loads `./openapi.json` and `./scalar.js`, both siblings in the built output. |
| `vendor/scalar-api-reference-<version>.js` | The Scalar standalone bundle, committed. The only third-party file here. |
| `dist/fonts/` | Inter, Bricolage Grotesque and JetBrains Mono, copied from `www/src/fonts` at build time so the docs wear the same clothes as mimux.dev. |
| `dist/` | **Committed build output.** This is what GitHub Pages serves, byte for byte. |

## Building

```sh
make docs        # assemble docs/dist
make docs-check  # assemble, then fail if the committed dist is stale
```

`make docs` runs [`scripts/docs.sh`](../scripts/docs.sh), which is a copy, not a
build: no npm, no bundler, no network. It wipes `docs/dist` and assembles it from

- `src/index.html` → `dist/index.html`
- `pro/openapi.json` → `dist/openapi.json`
- `vendor/scalar-api-reference-*.js` → `dist/scalar.js`
- a `dist/CNAME` containing `docs.mimux.dev`

To look at it locally, serve the directory — `file://` will not do, the page
fetches `openapi.json` over HTTP:

```sh
make docs && python3 -m http.server -d docs/dist 8080
```

## The staleness rule

**`docs/dist` is committed, and it must always match its sources.** The site is
deployed as-is; nothing regenerates it at deploy time, so a stale `dist` means
the published reference silently describes an older API.

The rule is enforced by the `staleness` job in
[`.github/workflows/docs.yml`](../.github/workflows/docs.yml): it runs `make
docs` and then `git diff --exit-code docs/dist`. A diff fails the build and the
deploy never runs.

In practice: **whenever you touch `pro/openapi.json`, run `make docs` and commit
`docs/dist` in the same commit.** `make docs-check` tells you if you forgot. It
is deliberately *not* wired into `make check` — CI owns that gate, and a stale
`dist` should not block an unrelated local check run.

## Why Scalar

Considered: Redoc, Swagger UI, Stoplight Elements, Scalar.

- **One standalone file, vendored.** Scalar ships a single self-contained
  browser bundle. It gets committed, the built site references the local copy,
  and the published page makes **zero network requests at runtime** — no CDN, no
  fonts, no telemetry needed to render. Nothing here can break because someone
  else's CDN had a bad day.
- **No backend, no build step.** The whole site is three files and a CNAME. CI
  runs `cp`, not `npm ci`.
- **Request samples in every language, generated from the spec.** curl, Python,
  JS, Go, Ruby, PHP — free, and correct by construction, which matters for an API
  whose whole audience is people wiring up a machine.
- **Modern reading UX.** Three-column layout, real search, dark mode by default,
  deep links per operation.

Redoc's free build has no request samples and no try-it. Swagger UI is dated and
noisier. Stoplight Elements wants a web component toolchain. Scalar was the
shortest path to a good page.

### Re-vendoring / upgrading

The bundle is pinned by filename. To move to a new version:

```sh
V=1.66.0   # whatever https://www.npmjs.com/package/@scalar/api-reference says
curl -fsSL "https://cdn.jsdelivr.net/npm/@scalar/api-reference@$V/dist/browser/standalone.js" \
  -o "docs/vendor/scalar-api-reference-$V.js"
rm docs/vendor/scalar-api-reference-<old>.js   # exactly one file must match the glob
make docs
```

Equivalently, without jsdelivr:

```sh
npm pack "@scalar/api-reference@$V"
tar -xzOf "scalar-api-reference-$V.tgz" package/dist/browser/standalone.js \
  > "docs/vendor/scalar-api-reference-$V.js"
```

`scripts/docs.sh` globs `scalar-api-reference-*.js`, so leaving two versions in
`vendor/` makes the build fail loudly rather than pick one at random. Check the
banner comment at the top of the file for the version it actually is, load the
page, and commit `docs/dist` along with the new bundle.

## Deployment

Pushes to `main` that touch `docs/**` or `pro/openapi.json` run
`.github/workflows/docs.yml`: the `staleness` job proves the committed output is
current, then `deploy` uploads `docs/dist` as a Pages artifact and publishes it.

### GitHub Pages setup (one-off)

1. **Settings → Pages → Build and deployment → Source: GitHub Actions.** Not
   "Deploy from a branch" — the workflow uploads an artifact.
2. **Custom domain: `docs.mimux.dev`.** The `CNAME` file in `docs/dist` carries
   it too, so the setting survives a redeploy.
3. **Tick "Enforce HTTPS"** once the certificate has been issued (it can take a
   few minutes after DNS resolves).

### DNS

One record at the `mimux.dev` registrar:

| Type | Name | Value |
|---|---|---|
| `CNAME` | `docs` | `mattmezza.github.io.` |

That is the *user* Pages host, not `mattmezza.github.io/mimux` — GitHub routes to
the right repository using the `CNAME` file in the artifact. Do not add an A
record; do not proxy it through a CDN that terminates TLS before GitHub, or
certificate issuance will fail.

### Known: this fails while the repo is private

**GitHub Pages requires a public repository** on free plans. Until mimux is
public, the `deploy` job fails with a Pages-not-enabled error. That is expected,
not a bug to chase — the `staleness` job still runs and still guards the
committed output, which is the half that matters before launch.
