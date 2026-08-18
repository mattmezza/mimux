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
import glob

css = open("www/dist/css/app.css").read()
# The inlined stylesheet lives at the root, so ../ paths must become absolute.
css = css.replace('url("../', 'url("/')
marker = '<link rel="stylesheet" href="/css/app.css">'
pages = glob.glob("www/dist/**/*.html", recursive=True)
assert pages, "no pages found in www/dist"
for page in pages:
    html = open(page).read()
    assert marker in html, f"stylesheet link marker missing from {page}"
    open(page, "w").write(html.replace(marker, "<style>\n" + css + "\n</style>"))
print(f"inlined CSS into {len(pages)} pages")
EOF
