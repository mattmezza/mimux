#!/bin/sh
# Assemble the static documentation site into docs/dist.
#
# This is a copy plus one stdlib-only Python pass, not a build: no npm, no
# bundler, no network, no dependency to install. The Scalar renderer is vendored
# in docs/vendor as a single committed file and the prose pages are plain HTML
# fragments, so this script is deterministic and its output is safe to commit
# and deploy as-is.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out=$root/docs/dist

rm -rf "$out"
mkdir -p "$out"

# Every page in docs/src/pages, wrapped in docs/src/shell.html with the shared
# navigation. index.html lands at the root; every other fragment gets its own
# directory, so no page ever links to a .html extension.
python3 "$root/scripts/docs_pages.py" "$root/docs/src" "$out"
cp "$root/docs/src/style.css" "$out/style.css"
# The spec is the one the pro binary embeds and serves at /api/v1/openapi.json.
# One source, two consumers — the docs can never describe a different API.
cp "$root/pro/openapi.json" "$out/openapi.json"
# Globbed rather than named so a version bump is a file swap, not a script edit.
# Two matching files make cp fail loudly, which is the right outcome.
cp "$root"/docs/vendor/scalar-api-reference-*.js "$out/scalar.js"
# The house fonts, shared with the marketing site so the two stay in step.
mkdir -p "$out/fonts"
cp "$root/www/src/fonts/inter-var.woff2" \
   "$root/www/src/fonts/bricolage-var.woff2" \
   "$root/www/src/fonts/jbm-400.woff2" \
   "$root/www/src/fonts/OFL-Inter.txt" \
   "$root/www/src/fonts/OFL-BricolageGrotesque.txt" \
   "$root/www/src/fonts/OFL-JetBrainsMono.txt" \
   "$out/fonts/"
# GitHub Pages reads the custom domain from this file in the published artifact.
echo docs.mimux.dev > "$out/CNAME"

echo "docs/dist assembled from docs/src, pro/openapi.json and docs/vendor."

