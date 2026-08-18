# www assets — generation log

Every generated image on mimux.dev came from the `pi` CLI driving the Runware
MCP tool (`runware:100@1`, FLUX.1-schnell, 4 steps), then post-processed with
ImageMagick. Commands below reproduce each one. Raw generator output lives in
`www/src/assets/` (not shipped to `dist/`).

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

## Icons — `src/favicon.svg` → `src/img/icon-*.png`, `favicon.ico`

The favicon is a hand-written SVG (cream stamp, ink border, vermilion "m").
All raster icons derive from it:

```bash
magick -background none -density 300 src/favicon.svg -resize 512x512 src/img/icon-512.png
magick src/img/icon-512.png -resize 192x192 src/img/icon-192.png
magick src/img/icon-512.png -resize 180x180 src/img/icon-180.png
cp src/img/icon-180.png src/img/apple-touch-icon.png
magick src/img/icon-512.png -define icon:auto-resize=48,32,16 src/img/favicon.ico
```

