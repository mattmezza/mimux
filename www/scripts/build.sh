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

# The script is content-addressed so it can be cached forever: HTML is
# short-cached and updates promptly, but a year-long /js/site.js meant a
# returning visitor got new markup driven by last month's JavaScript — dead
# buttons that work perfectly on a hard refresh and nowhere else.
js = open("www/dist/js/site.js", "rb").read()
js_name = "site.%s.js" % hashlib.sha256(js).hexdigest()[:8]
open("www/dist/js/" + js_name, "wb").write(js)
js_marker = '<script defer src="/js/site.js"></script>'
js_tag = '<script defer src="/js/%s"></script>' % js_name
# www/dist/js/site.js stays, byte-identical. HTML already sitting in browser
# and edge caches still asks for it; delete it and those visitors get a 404
# until their copy expires. Not leftovers — do not tidy it away.

pages = glob.glob("www/dist/**/*.html", recursive=True)
assert pages, "no pages found in www/dist"
scripted = 0
for page in pages:
    html = open(page).read()
    assert marker in html, f"stylesheet link marker missing from {page}"
    # style-src 'self' does NOT cover inline style attributes, and no hash can
    # whitelist them. Catching them here is the only thing standing between a
    # stray style="" and a silently unstyled production page.
    stray = re.search(r'<[^>]+\sstyle="', html)
    assert not stray, f"{page}: inline style attribute is blocked by the CSP: {stray.group(0)}"
    html = html.replace(marker, "<style>" + block + "</style>")
    html = html.replace(js_marker, js_tag)
    # 404.html carries no script, so the tag is optional — but any surviving
    # mention of the unhashed path means the tag was written in some shape this
    # replace did not match, and would ship a page pinned to a cacheable URL.
    assert "/js/site.js" not in html, f"{page}: script tag does not match {js_marker}"
    scripted += js_tag in html
    open(page, "w").write(html)
assert scripted, "no page references /js/site.js"
print(f"inlined CSS into {len(pages)} pages, pinned {scripted} script tags to /js/{js_name}")

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
