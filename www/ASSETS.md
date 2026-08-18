# www assets — generation log

Every generated image on mimux.dev came from the `pi` CLI driving the Runware
MCP tool (`runware:100@1`, FLUX.1-schnell, 4 steps), then post-processed with
ImageMagick. Commands below reproduce each one. Raw generator output lives in
`www/src/assets/` (not shipped to `dist/`).

## OG / social card — `src/img/og.png` (exactly 1200×630)

Generated 2026-08-18 with a direct Runware REST call (FLUX.1-schnell,
`runware:100@1`, 4 steps, 1216×640, 2 candidates), then hue-shifted toward the
brand indigo and cropped:

```bash
curl -s -X POST https://api.runware.ai/v1 -H "Content-Type: application/json" -d '[
  {"taskType":"authentication","apiKey":"'$RUNWARE_API_KEY'"},
  {"taskType":"imageInference","taskUUID":"<uuid>",
   "positivePrompt":"minimal dark technical vector artwork for a social banner: a single luminous indigo violet envelope glyph slightly left of center on a near-black charcoal background, surrounded by thin concentric orbit rings with small glowing geometric nodes, one node connected to the envelope by a fine bright line, faint hairline dot grid across the background, generous negative space on the right, flat vector style, crisp edges, high contrast, subtle glow, no text, no letters, no words, no watermark",
   "width":1216,"height":640,"model":"runware:100@1","steps":4,"numberResults":2}]'
# picked the centred-composition candidate (raw: src/assets/og-raw-v2.jpg)
magick og-raw-v2.jpg -modulate 100,92,93 og-shift.png
magick og-shift.png -gravity center -crop 1200x630+0+0 +repage \
  -dither FloydSteinberg -colors 64 -strip PNG8:src/img/og.png
```

The 2026-08 redesign removed the generated hero texture (`hero-*.{jpg,webp}`)
— all page backdrops are now pure CSS gradients in `app.css`, so no image can
ever be the LCP element.

## Icon source — `src/img/icon-{512,192,180}.png`, `apple-touch-icon.png`, `favicon.ico`

```bash
pi -p "Generate a 512x512 image: app icon: a geometric lowercase letter m \
formed from angular envelope-shaped facets, indigo violet gradient glyph \
centered on very dark charcoal rounded-square background, flat minimal, crisp \
edges, minimal dark technical abstract artwork, near-black charcoal \
background, single indigo violet accent color, fine thin hairline grid lines, \
generous negative space, flat vector style, high contrast, no text, no \
letters, no words, no watermark. Use the mcp tool with tool='run', args: \
model='runware:100@1', positivePrompt='<prompt>', width=512, height=512, \
steps=4, numberResults=1. Download result to ./icon-raw.jpg."

for s in 512 192 180; do
  magick src/assets/icon-raw.jpg -resize ${s}x${s} -strip src/img/icon-${s}.png
done
cp src/img/icon-180.png src/img/apple-touch-icon.png
magick src/assets/icon-raw.jpg \
  \( -clone 0 -resize 16x16 \) \( -clone 0 -resize 32x32 \) \( -clone 0 -resize 48x48 \) \
  -delete 0 -strip src/img/favicon.ico
```

`src/favicon.svg` is hand-drawn (not generated) to match the generated mark:
same angular "m" glyph, same indigo gradient, on the ink-900 rounded square.

## Notes

- No text is baked into any generated image; all type is HTML.
- Generator hue drift is real: FLUX.1-schnell reads "indigo" as magenta about
  half the time. The `-modulate 100,100,70` hue rotation above is the fix —
  keep it if regenerating.
- The OG card must stay exactly 1200×630 PNG. Never ship the generator's
  native aspect ratio.
