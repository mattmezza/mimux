# www — mimux.dev

The marketing site for mimux. Static HTML, Tailwind v4 (CSS-first config, no
`tailwind.config.js`), zero JavaScript of our own. Deployed to Cloudflare
Pages at <https://mimux.dev>.

## Layout

```
www/
  src/                  # source of truth
    index.html          # home
    features/ agents/ pricing/ about/    # marketing pages
    privacy/ terms/ security/ legal/     # policy pages
    404.html            # Cloudflare Pages not-found page
    app.css             # Tailwind v4 entry: @theme tokens + components
    js/site.js          # progressive enhancement: reveal, copy, mobile nav
    fonts/              # self-hosted Inter + Space Grotesk + JetBrains Mono (OFL)
    img/                # icons, og.png (see ASSETS.md)
    assets/             # raw generator output — source material, NOT shipped
    favicon.svg, site.webmanifest, robots.txt, sitemap.xml
    llms.txt, llms-full.txt
    _headers            # Cloudflare Pages headers (CSP, caching)
  scripts/
    build.sh            # www-build implementation (inlines CSS into every page)
    serve.py            # local static server with gzip (Lighthouse-realistic)
  dist/                 # build output, committed, deployed as-is
  ASSETS.md             # every image-generation command, reproducible
```

The shared header/footer shell is duplicated verbatim across pages on purpose —
no templating step, no build dependency. Editing the nav or footer means
editing it in every `index.html` (10 files); `grep -rl "footer-mark" src` lists
them.

## Develop

```sh
make www        # tailwind --watch + local server on http://localhost:8732
```

Edits to `src/index.html` need a rebuild of `dist/` — the dev target only
watches CSS. Re-run `make www` after HTML changes (or `make www-build`).

## Build

```sh
make www-build  # → www/dist/
```

`build.sh` copies `src/` to `dist/`, compiles Tailwind minified, then inlines
the stylesheet into `index.html` — the deployed page has zero render-blocking
requests (a standalone `css/app.css` is still emitted for debugging).

## Deploy

Cloudflare Pages via `.github/workflows/www.yml`:

- **Pull requests touching `www/**`** build `dist/` and run sanity checks, no
  deploy.
- **Release deploys:** `make release name=www-v0.1` — the workflow only deploys
  for tags starting `www-v`. Client releases (`v*`) are handled by
  `release.yml`, which is guarded to ignore `www-v*` tags.

Secrets needed in GitHub: `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`.
Pages project name: `mimux-www`.

## Swapping the screenshot placeholders

Four slots in `index.html`, each marked with an HTML comment:

- `SCREENSHOT-PLACEHOLDER: unified-inbox|reading-pane|compose` — three dashed
  frames in `#screenshots`. Replace each `<figure class="shot-frame">` with:

  ```html
  <img src="/img/shots/unified-inbox.webp" alt="The mimux unified inbox, three accounts merged into one message list"
       width="1600" height="1000" loading="lazy">
  ```

  Capture at 1600×1000, export WebP (quality ~80) into `www/src/img/shots/`,
  keep explicit `width`/`height` and `loading="lazy"`.

- `DEMO-GIF-PLACEHOLDER: agent-triage` — the 30-second agent demo. Replace the
  frame with a `<video>` (muted, loop, playsinline, poster) or an `<img>` GIF
  ≤1200×750. Keep it under ~2MB.

## Analytics

One external request: Umami at `analytics.casa.merola.co` (self-hosted,
cookieless, DNT honoured, `data-domains` restricted to mimux.dev). The website
ID is hardcoded in `index.html` — search for `WEBSITE_ID` and replace it with
the UUID from the Umami dashboard before first deploy. The footer copy claims
"exactly one external request" — keep that true.

No `/s.js` first-party proxy: decided against it — the Pages `_redirects`
proxy would also need to forward Umami's `/api/send` beacon endpoint, and the
recovered ad-blocked traffic isn't worth the moving part at launch. Revisit if
the numbers say otherwise; the CSP in `_headers` already permits the origin.

## Fonts

Subsetted (latin + punctuation) Inter variable and JetBrains Mono 400/500/700,
self-hosted under OFL — licence texts ship in `dist/fonts/`. Rebuild with
`pyftsubset` if the charset needs to grow (see command history in git).
