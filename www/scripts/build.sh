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
css = open("www/dist/css/app.css").read()
# The inlined stylesheet lives at the root, so ../ paths must become absolute.
css = css.replace('url("../', 'url("/')
html = open("www/dist/index.html").read()
marker = '<link rel="stylesheet" href="/css/app.css">'
assert marker in html, "stylesheet link marker missing from index.html"
html = html.replace(marker, "<style>\n" + css + "\n</style>")
open("www/dist/index.html", "w").write(html)
EOF
