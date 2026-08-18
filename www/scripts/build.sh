#!/bin/sh
# Build www/dist from www/src: copy static assets, compile Tailwind, then
# inline the stylesheet into index.html so the page has zero render-blocking
# requests. The standalone css/app.css is still emitted for debugging.
set -eu

cd "$(dirname "$0")/../.."

mkdir -p www/dist/css
rsync -a --delete --exclude='app.css' --exclude='assets/' www/src/ www/dist/
npx @tailwindcss/cli -i www/src/app.css -o www/dist/css/app.css --minify

python3 - <<'EOF'
import base64, glob, hashlib, re

css = open("www/dist/css/app.css").read()
# The inlined stylesheet lives at the root, so ../ paths must become absolute.
css = css.replace('url("../', 'url("/')
marker = '<link rel="stylesheet" href="/css/app.css">'
block = "\n" + css + "\n"
pages = glob.glob("www/dist/**/*.html", recursive=True)
assert pages, "no pages found in www/dist"
for page in pages:
    html = open(page).read()
    assert marker in html, f"stylesheet link marker missing from {page}"
    # style-src 'self' does NOT cover inline style attributes, and no hash can
    # whitelist them. Catching them here is the only thing standing between a
    # stray style="" and a silently unstyled production page.
    stray = re.search(r'<[^>]+\sstyle="', html)
    assert not stray, f"{page}: inline style attribute is blocked by the CSP: {stray.group(0)}"
    open(page, "w").write(html.replace(marker, "<style>" + block + "</style>"))
print(f"inlined CSS into {len(pages)} pages")

# The CSP in _headers must whitelist the exact block we just inlined. Computed
# here rather than written by hand: the stylesheet changes on every build, and a
# stale hash blocks the stylesheet outright, which loses all styling on a page
# that still returns 200 and so looks fine to every check that is not a browser.
digest = base64.b64encode(hashlib.sha256(block.encode()).digest()).decode()
headers = open("www/dist/_headers").read()
assert "style-src 'self'" in headers, "_headers: no style-src 'self' to extend"
headers = headers.replace("style-src 'self'", f"style-src 'self' 'sha256-{digest}'", 1)
open("www/dist/_headers", "w").write(headers)
print(f"CSP style-src pinned to sha256-{digest}")
EOF
