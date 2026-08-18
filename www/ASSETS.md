# www assets — generation log

Every generated image on mimux.dev came from the `pi` CLI driving the Runware
MCP tool (`runware:100@1`, FLUX.1-schnell, 4 steps), then post-processed with
ImageMagick. Commands below reproduce each one. Raw generator output lives in
`www/src/assets/` (not shipped to `dist/`).

## OG / social card — `src/img/og.png` (exactly 1200×630)

```bash
pi -p "Generate a 1216x640 image: abstract composition representing self-hosted \
email: a single glowing indigo envelope glyph at the center, concentric thin \
orbit lines and small geometric nodes around it, minimal dark technical \
abstract artwork, near-black charcoal background, single indigo violet accent \
color, fine thin hairline grid lines, generous negative space, flat vector \
style, high contrast, no text, no letters, no words, no watermark. Use the mcp \
tool with tool='run', args: model='runware:100@1', positivePrompt='<prompt>', \
width=1216, height=640, steps=4, numberResults=1. Download result to ./og-raw.jpg."

# 1216x640 is the nearest supported size; crop to the exact OG spec.
magick src/assets/og-raw.jpg -gravity center -crop 1200x630+0+0 +repage \
  -dither FloydSteinberg -colors 64 -strip PNG8:src/img/og.png
```

## Hero texture — `src/img/hero-1536.{webp,jpg}`, `hero-1024.{webp,jpg}`

```bash
pi -p "Generate a 1536x640 image: wide dark abstract banner, deep charcoal \
near-black background covered edge to edge with faint horizontal rows of \
glowing message-list scanlines, one single row highlighted in bright indigo \
violet (#6366f1) with a soft glow, thin hairline grid, terminal aesthetic, \
clearly visible scanlines across the whole width, flat vector style, crisp \
edges, no text, no letters, no watermark. Use the mcp tool with tool='run', \
args: model='runware:100@1', positivePrompt='<prompt>', width=1536, \
height=640, steps=4, numberResults=1. Download result to ./hero-raw.jpg."

# The generator drifted magenta; rotate hue into the indigo/violet brand hue.
magick src/assets/hero-raw.jpg -modulate 100,100,70 src/assets/hero-shift.png

magick src/assets/hero-shift.png -strip -quality 78 src/img/hero-1536.jpg
magick src/assets/hero-shift.png -resize 1024x -strip -quality 78 src/img/hero-1024.jpg
magick src/assets/hero-shift.png -strip -quality 82 -define webp:method=6 src/img/hero-1536.webp
magick src/assets/hero-shift.png -resize 1024x -strip -quality 82 -define webp:method=6 src/img/hero-1024.webp
```

Used as a CSS background (`.hero-texture`), never an `<img>`, so it cannot
become the LCP element.

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
