#!/usr/bin/env bash
set -euo pipefail

command_name="${1:-}"
case "$command_name" in
  build | serve | preview) ;;
  *)
    printf 'usage: %s build|serve [zensical args...] | preview [port]\n' "$0" >&2
    exit 2
    ;;
esac
shift || true

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$script_dir"
# The build case removes this directory outright, so it must stay a plain
# relative path inside docs/: no absolute paths, no "."/".." traversal.
site_dir="${ROBOREV_DOCS_SITE_DIR:-site}"
if [[ -z "$site_dir" || "$site_dir" == /* ]]; then
  printf 'ROBOREV_DOCS_SITE_DIR must be relative to docs/: %s\n' "$site_dir" >&2
  exit 2
fi
IFS='/' read -r -a site_dir_parts <<< "$site_dir"
for site_dir_part in "${site_dir_parts[@]}"; do
  if [[ -z "$site_dir_part" || "$site_dir_part" == "." || "$site_dir_part" == ".." ]]; then
    printf 'ROBOREV_DOCS_SITE_DIR must not contain empty, ".", or ".." segments: %s\n' \
      "$site_dir" >&2
    exit 2
  fi
done

if [[ -n "${VIRTUAL_ENV:-}" && -x "$VIRTUAL_ENV/bin/zensical" ]]; then
  zensical_bin="$VIRTUAL_ENV/bin/zensical"
elif [[ -x "$docs_root/.venv/bin/zensical" ]]; then
  zensical_bin="$docs_root/.venv/bin/zensical"
elif command -v zensical >/dev/null 2>&1; then
  zensical_bin="zensical"
else
  printf 'zensical not found; install with: cd docs && uv sync --frozen --no-dev\n' >&2
  exit 127
fi

tmp_docs=""
tmp_config_base=""
tmp_config=""

cleanup() {
  if [[ -n "$tmp_docs" ]]; then
    rm -rf "$tmp_docs"
  fi
  if [[ -n "$tmp_config" ]]; then
    rm -f "$tmp_config"
  fi
  if [[ -n "$tmp_config_base" ]]; then
    rm -f "$tmp_config_base"
  fi
}
trap cleanup EXIT INT TERM

tmp_docs_name="$(cd "$docs_root" && mktemp -d zensical-public-docs.XXXXXX)"
tmp_docs="$docs_root/$tmp_docs_name"
tmp_config_base_name="$(cd "$docs_root" && mktemp .zensical-build.XXXXXX)"
tmp_config_base="$docs_root/$tmp_config_base_name"
tmp_config="$tmp_config_base.toml"
tmp_config_name="$tmp_config_base_name.toml"
if [[ -e "$tmp_config" ]]; then
  printf 'temporary config path already exists: %s\n' "$tmp_config" >&2
  exit 1
fi
mv "$tmp_config_base" "$tmp_config"
tmp_config_base=""

(
  cd "$docs_root"
  tar \
    --exclude './.venv' \
    --exclude './.vercel' \
    --exclude './.env*.local' \
    --exclude './site' \
    --exclude './zensical-public-docs.*' \
    --exclude './.zensical-build.*' \
    --exclude './.ruff_cache' \
    --exclude './.mypy_cache' \
    --exclude './superpowers' \
    --exclude './overrides' \
    --exclude './website' \
    --exclude './llms.txt' \
    --exclude './scripts' \
    --exclude './screenshots' \
    --exclude './README.md' \
    --exclude './pyproject.toml' \
    --exclude './uv.lock' \
    --exclude './vercel.json' \
    --exclude './vercel-build.sh' \
    --exclude './zensical-docs.sh' \
    --exclude './zensical.toml' \
    --exclude './assets/*.sh' \
    -cf - .
) | (cd "$tmp_docs" && tar -xf -)
# The docs tier renders under /docs/; the hand-written website tier owns the
# site root.
docs_output_dir="$site_dir/docs"

awk -v docs_dir="$tmp_docs_name" -v site_dir="$docs_output_dir" '
  $0 == "docs_dir = \"docs\"" {
    print "docs_dir = \"" docs_dir "\""
    next
  }
  $0 == "site_dir = \"site\"" {
    print "site_dir = \"" site_dir "\""
    next
  }
  { print }
' "$docs_root/zensical.toml" > "$tmp_config"

assemble_site_root() {
  local entry
  for entry in index.html index.md guide guide.md 404.html \
    styles scripts fonts; do
    if [[ ! -e "$docs_root/website/$entry" ]]; then
      printf 'missing website tier entry: %s\n' "$docs_root/website/$entry" >&2
      exit 1
    fi
    cp -R "$docs_root/website/$entry" "$docs_root/$site_dir/$entry"
  done

  if [[ ! -s "$docs_root/llms.txt" ]]; then
    printf 'missing docs/llms.txt\n' >&2
    exit 1
  fi
  cp "$docs_root/llms.txt" "$docs_root/$site_dir/llms.txt"

  # Image media lives on the docs asset branches, never on main, so the root
  # favicon is the hydrated docs favicon rather than a tracked file.
  local favicon="$docs_root/$docs_output_dir/assets/static/favicon.svg"
  if [[ ! -f "$favicon" ]]; then
    printf 'missing hydrated favicon: %s\n' "$favicon" >&2
    exit 1
  fi
  cp "$favicon" "$docs_root/$site_dir/favicon.svg"

  # The website copy is recursive over the working tree; keep editor and OS
  # detritus out of the published root.
  find "$docs_root/$site_dir" -maxdepth 3 \
    \( -name '.DS_Store' -o -name '*~' -o -name '*.swp' \) -delete

  # Crawlers expect the sitemap at the origin root; Zensical's instant
  # navigation also fetches /docs/sitemap.xml, so keep both copies.
  local sitemap
  for sitemap in sitemap.xml sitemap.xml.gz; do
    if [[ -f "$docs_root/$docs_output_dir/$sitemap" ]]; then
      cp "$docs_root/$docs_output_dir/$sitemap" "$docs_root/$site_dir/$sitemap"
    fi
  done
}

build_site() {
  # Zensical only cleans its own site/docs subtree; clear the whole output
  # directory so files from earlier builds or layouts never ship.
  rm -rf "${docs_root:?}/${site_dir:?}"
  (cd "$docs_root" && "$zensical_bin" build --strict --config-file "$tmp_config_name" "$@")
  (
    cd "$docs_root"
    python3 scripts/copy_public_markdown_sources.py \
      --docs-dir "$tmp_docs_name" \
      --site-dir "$docs_output_dir" \
      --config "$tmp_config_name"
  )
  assemble_site_root
}

case "$command_name" in
  build)
    build_site "$@"
    ;;
  serve)
    # Zensical mounts the docs at the site_url path, so this previews the docs
    # tier at /docs/ with live reload. The product page and guide are static
    # files outside Zensical; use "preview" to see the whole site.
    (cd "$docs_root" && "$zensical_bin" serve --config-file "$tmp_config_name" "$@")
    ;;
  preview)
    build_site
    preview_port="${1:-8000}"
    printf 'Serving %s on http://127.0.0.1:%s (production routing, no live reload)\n' \
      "$site_dir" "$preview_port"
    python3 -m http.server --bind 127.0.0.1 --directory "$docs_root/$site_dir" "$preview_port"
    ;;
esac
