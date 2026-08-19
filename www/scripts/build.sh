#!/bin/sh
# Build www/dist from www/src: copy static assets, compile Tailwind, then
# inline the stylesheet into index.html so the page has zero render-blocking
# requests. The standalone css/app.css is still emitted for debugging.
set -eu

cd "$(dirname "$0")/../.."

mkdir -p www/dist/css
# js/site.*.js is excluded, which in rsync also protects it from --delete: the
# previous build's bundle has to survive this one. HTML already sitting in a
# browser or edge cache is pinned to that exact filename, and deleting it turns
# every one of those pages into a 404 for the only script it has. They are
# pruned by age further down instead.
rsync -a --delete --exclude='app.css' --exclude='assets/' --exclude='js/site.*.js' \
	www/src/ www/dist/
npx @tailwindcss/cli -i www/src/app.css -o www/dist/css/app.css --minify

python3 - <<'EOF'
import base64, glob, hashlib, json, os, re, time

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

# Same reasoning for the older *hashed* bundles the rsync above preserved, with
# an expiry: HTML is served must-revalidate (see _headers), so nothing can be
# pinned to a month-old bundle unless the visitor has been offline that long.
# Age rather than a count because this must be idempotent — building twice in a
# row, which is exactly what a release does after a local build, must not throw
# away the bundle the live site is still pointing at.
KEEP = 30 * 86400
for old in glob.glob("www/dist/js/site.*.js"):
    if os.path.basename(old) != js_name and time.time() - os.path.getmtime(old) > KEEP:
        os.remove(old)
        print("pruned " + old + " — older than 30 days, nothing can still ask for it")

pages = glob.glob("www/dist/**/*.html", recursive=True)
assert pages, "no pages found in www/dist"

# Prices come from account/pricing.json — the same file the shop embeds and
# charges from. The site used to carry its own hand-typed figures, which is a
# quote the checkout is under no obligation to honour. www/src keeps tokens
# ({{price:annual:eur}} → €49, {{amount:…}} → the bare number for JSON-LD) and
# they are stamped in here, so a page that cannot be priced fails the build
# instead of shipping last year's number.
pricing = json.load(open("account/pricing.json"))
symbols = {c["code"]: c["symbol"] for c in pricing["currencies"]}
TOKEN = re.compile(r"\{\{(price|amount):([a-z]+):([a-z]+)\}\}")

def stamp_prices(path, text):
    def one(m):
        kind, plan, code = m.groups()
        cents = pricing["plans"].get(plan, {}).get(code)
        if cents is None or code not in symbols:
            raise SystemExit(f"{path}: {m.group(0)} is not in account/pricing.json")
        figure = f"{cents // 100}" if cents % 100 == 0 else f"{cents // 100}.{cents % 100:02d}"
        return (symbols[code] + figure) if kind == "price" else figure
    text = TOKEN.sub(one, text)
    # A token the regex did not match (a typo, a missing field) would otherwise
    # be served to a customer verbatim.
    assert "{{" not in text, f"{path}: unresolved template token"
    return text

# The tokens are the only sanctioned way a price reaches a page: a figure typed
# straight into www/src is a number nothing keeps in step with pricing.json.
# €0/$0 is the free client, which the shop has no plan for and never will.
priced = glob.glob("www/src/**/*.html", recursive=True) + glob.glob("www/src/*.txt")
for src in priced:
    for hit in re.finditer(r"[€$]\s?\d+(?:[.,]\d+)*", open(src).read()):
        assert hit.group(0) in ("€0", "$0"), \
            f"{src}: hardcoded price {hit.group(0)} — use a {{{{price:plan:currency}}}} token"

for txt in ["www/dist/llms.txt", "www/dist/llms-full.txt"]:
    stamped = stamp_prices(txt, open(txt).read())
    open(txt, "w").write(stamped)

scripted = 0
for page in pages:
    html = stamp_prices(page, open(page).read())
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
