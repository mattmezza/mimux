# www assets — generation log

Every generated image on mimux.dev came from the `pi` CLI driving the Runware
MCP tool (`runware:100@1`, FLUX.1-schnell, 4 steps), then post-processed with
ImageMagick. Commands below reproduce each one. Source material lives in
`www/src/assets/` (not shipped to `dist/`) and is kept to what a page still
derives from, plus `og-previous.png` for comparing a new card against the one
it replaces — the rest of the iteration history is in git.

## OG / social card — `src/img/og.png` (exactly 1200×630)

Hand-drawn vector, not generated: `src/assets/og-postal.svg` (airmail bands,
envelope, perforated stamp carrying the favicon "m", textless postmark).
Generated candidates (FLUX schnell via the Runware REST API) were rejected
because the model invents gibberish text inside postmarks. Render:

```bash
magick -background none -density 150 src/assets/og-postal.svg \
  -resize 1200x630 -strip og.png
magick og.png +dither -colors 256 -strip PNG8:src/img/og.png
```

The 2026-08 postal redesign uses no raster imagery at all on the pages —
every backdrop (ruled lines, airmail stripes, scallops, postmark) is CSS or
inline SVG, so no image can ever be the LCP element.

## Icons — from the app logo

`www/src/img/icon-512.png` is a straight copy of the product logo at
`web/static/icons/icon-512.png` (dark square, indigo envelope-tray, green
leaf). `src/favicon.svg` is a hand-traced vector of the same mark (geometry
measured off the PNG). All other raster icons derive from the 512:

```bash
cp web/static/icons/icon-512.png www/src/img/icon-512.png
magick www/src/img/icon-512.png -resize 192x192 www/src/img/icon-192.png
magick www/src/img/icon-512.png -resize 180x180 www/src/img/apple-touch-icon.png
magick www/src/img/icon-512.png -define icon:auto-resize=48,32,16 www/src/img/favicon.ico
```

The docs shell (docs/src/index.html) embeds the same mark as inline data-URI
SVGs for its favicon and header brand.
