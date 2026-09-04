#!/usr/bin/env bash
set -euo pipefail

failed=0

fail() {
  printf '%s\n' "$1" >&2
  failed=1
}

if [[ -e "zensical.toml" ]]; then
  fail 'Zensical config must live under docs/: zensical.toml'
fi

if [[ -e "vercel.json" ]]; then
  fail 'Vercel config must live under docs/: vercel.json'
fi

tracked_media="$(
  git ls-files docs 2>/dev/null | grep -E '\.(png|svg|jpg|jpeg|webp|gif)$' || true
)"
if [[ -n "$tracked_media" ]]; then
  printf 'docs image media must live in docs asset branches, not main:\n%s\n' "$tracked_media" >&2
  failed=1
fi

tracked_hydrated_assets="$(
  git ls-files docs/assets/static docs/assets/generated 2>/dev/null || true
)"
if [[ -n "$tracked_hydrated_assets" ]]; then
  printf 'hydrated docs assets must be ignored, not tracked:\n%s\n' "$tracked_hydrated_assets" >&2
  failed=1
fi

if [[ "$failed" -ne 0 ]]; then
  exit 1
fi

root_media_refs="$(
  (rg -n '(<img[^>]+src="/|!\[[^]]*\]\(/)[^)" >]+\.(png|svg|jpg|jpeg|webp|gif)' docs README.md || true) \
    | grep -v '/docs/assets/static/' \
    | grep -v '/docs/assets/generated/' \
    || true
)"
if [[ -n "$root_media_refs" ]]; then
  printf 'docs media references must use /docs/assets/static or /docs/assets/generated:\n%s\n' "$root_media_refs" >&2
  exit 1
fi

website_entries=(
  "docs/website/index.html"
  "docs/website/index.md"
  "docs/website/guide/index.html"
  "docs/website/guide.md"
  "docs/website/404.html"
  "docs/website/styles/site.css"
  "docs/website/scripts/site.js"
  "docs/website/fonts/licenses/Inter-OFL-1.1.txt"
  "docs/website/fonts/licenses/JetBrains-Mono-OFL-1.1.txt"
  "docs/llms.txt"
)
for entry in "${website_entries[@]}"; do
  if [[ ! -f "$entry" ]]; then
    printf 'missing website tier file: %s\n' "$entry" >&2
    exit 1
  fi
done

bash docs/assets/hydrate-assets.sh

if command -v uv >/dev/null 2>&1; then
  (
    cd docs
    uv run --frozen bash ./zensical-docs.sh build
    uv run --frozen python scripts/check_built_site.py
    uv run --frozen python scripts/check_public_markdown_sources.py
    uv run --frozen python scripts/check_vercel_redirects.py
  )
else
  printf 'uv not found; skipping docs build validation\n' >&2
fi
