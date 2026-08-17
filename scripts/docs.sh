#!/bin/sh
# Assemble the static API reference site into docs/dist.
#
# This is a copy, not a build: no npm, no bundler, no network. The Scalar
# renderer is vendored in docs/vendor as a single committed file, so this script
# is deterministic and its output is safe to commit and deploy as-is.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out=$root/docs/dist

rm -rf "$out"
mkdir -p "$out"

cp "$root/docs/src/index.html" "$out/index.html"
# The spec is the one the pro binary embeds and serves at /api/v1/openapi.json.
# One source, two consumers — the docs can never describe a different API.
cp "$root/pro/openapi.json" "$out/openapi.json"
# Globbed rather than named so a version bump is a file swap, not a script edit.
# Two matching files make cp fail loudly, which is the right outcome.
cp "$root"/docs/vendor/scalar-api-reference-*.js "$out/scalar.js"
# GitHub Pages reads the custom domain from this file in the published artifact.
echo docs.mimux.dev > "$out/CNAME"

echo "docs/dist assembled from docs/src, pro/openapi.json and docs/vendor."
