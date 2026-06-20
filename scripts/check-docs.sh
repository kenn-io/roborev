#!/usr/bin/env bash
set -euo pipefail

missing=0

required_files=(
  "docs/index.md"
  "docs/quickstart.md"
  "docs/installation.md"
  "docs/commands.md"
  "docs/configuration.md"
  "docs/integrations/tui.md"
  "docs/integrations/github.md"
  "docs/integrations/kata.md"
  "docs/integrations/claudechic.md"
  "docs/guides/reviewing-code.md"
  "docs/guides/responding-to-reviews.md"
  "docs/guides/agent-skills.md"
  "docs/guides/assisted-refactoring.md"
  "docs/guides/auto-fixing.md"
  "docs/guides/repository-management.md"
  "docs/guides/hooks.md"
  "docs/guides/troubleshooting.md"
  "docs/advanced/background-tasks.md"
  "docs/advanced/subagent-review-panels.md"
  "docs/advanced/custom-tasks.md"
  "docs/advanced/acp.md"
  "docs/advanced/postgres-sync.md"
  "docs/advanced/streaming.md"
  "docs/agents/index.md"
  "docs/agent-hook.md"
  "docs/development.md"
  "docs/changelog.md"
  "docs/zensical.toml"
  "docs/zensical-docs.sh"
  "docs/pyproject.toml"
  "docs/uv.lock"
  "docs/vercel.json"
  "docs/vercel-build.sh"
  "docs/stylesheets/extra.css"
  "docs/javascripts/lightbox.js"
  "docs/scripts/check_built_site.py"
  "docs/scripts/check_vercel_redirects.py"
  "docs/assets/hydrate-assets.sh"
  "docs/assets/update-static-assets-branch.sh"
  "docs/screenshots/update-generated-assets-branch.sh"
  "scripts/update-docs.sh"
)

fail_missing() {
  printf 'missing required docs file: %s\n' "$1" >&2
  missing=1
}

for file in "${required_files[@]}"; do
  [[ -f "$file" ]] || fail_missing "$file"
done

if [[ -e "zensical.toml" ]]; then
  printf 'Zensical config must live under docs/: zensical.toml\n' >&2
  missing=1
fi

if [[ -e "vercel.json" ]]; then
  printf 'Vercel config must live under docs/: vercel.json\n' >&2
  missing=1
fi

tracked_media="$(
  git ls-files docs 2>/dev/null | grep -E '\.(png|svg|jpg|jpeg|webp|gif)$' || true
)"
if [[ -n "$tracked_media" ]]; then
  printf 'docs image media must live in docs asset branches, not main:\n%s\n' "$tracked_media" >&2
  missing=1
fi

tracked_hydrated_assets="$(
  git ls-files docs/assets/static docs/assets/generated 2>/dev/null || true
)"
if [[ -n "$tracked_hydrated_assets" ]]; then
  printf 'hydrated docs assets must be ignored, not tracked:\n%s\n' "$tracked_hydrated_assets" >&2
  missing=1
fi

if [[ "$missing" -ne 0 ]]; then
  exit 1
fi

require_line() {
  local file="$1"
  local expected="$2"

  if ! grep -F -- "$expected" "$file" >/dev/null; then
    printf 'missing required docs content in %s: %s\n' "$file" "$expected" >&2
    exit 1
  fi
}

reject_line() {
  local file="$1"
  local unexpected="$2"

  if grep -F -- "$unexpected" "$file" >/dev/null; then
    printf 'stale docs content in %s: %s\n' "$file" "$unexpected" >&2
    exit 1
  fi
}

require_line Makefile 'docs-install:'
require_line Makefile 'docs-build:'
require_line Makefile 'docs-serve:'
require_line Makefile 'docs-check:'
require_line Makefile 'docs-assets-branch:'
require_line Makefile 'docs-generated-assets-branch:'
require_line Makefile 'docs-deploy:'
require_line Makefile 'vercel deploy --prod'

require_line docs/vercel.json '"framework": null'
require_line docs/vercel.json '"installCommand": "uv sync --frozen --no-dev"'
require_line docs/vercel.json '"buildCommand": "uv run --frozen bash ./vercel-build.sh"'
require_line docs/vercel.json '"outputDirectory": "site"'
require_line docs/vercel-build.sh 'assets/hydrate-assets.sh'
require_line docs/vercel-build.sh './zensical-docs.sh" build'
require_line docs/pyproject.toml 'requires-python = ">=3.12"'
require_line docs/pyproject.toml '"zensical==0.0.43"'
require_line docs/pyproject.toml 'package = false'
require_line docs/zensical.toml 'site_name = "roborev"'
require_line docs/zensical.toml 'site_url = "https://roborev.io"'
require_line docs/zensical.toml 'docs_dir = "docs"'
require_line docs/zensical.toml 'site_dir = "site"'
require_line docs/zensical.toml 'logo = "assets/static/logo.svg"'
require_line docs/zensical.toml 'favicon = "assets/static/favicon.svg"'
require_line docs/assets/hydrate-assets.sh 'docs-assets'
require_line docs/assets/hydrate-assets.sh 'docs-generated-assets'
require_line docs/assets/update-static-assets-branch.sh 'docs-assets'
require_line docs/screenshots/update-generated-assets-branch.sh 'docs-generated-assets'
require_line scripts/update-docs.sh 'make docs-deploy'
require_line scripts/update-docs.sh 'docs-generated-assets'

reject_line README.md 'roborev-docs'

root_media_refs="$(
  rg -n '(<img[^>]+src="/|!\[[^]]*\]\(/)[^)" >]+\.(png|svg|jpg|jpeg|webp|gif)' docs README.md || true
)"
if [[ -n "$root_media_refs" ]]; then
  printf 'docs media references must use /assets/static or /assets/generated:\n%s\n' "$root_media_refs" >&2
  exit 1
fi

bash docs/assets/hydrate-assets.sh

if command -v uv >/dev/null 2>&1; then
  (
    cd docs
    uv run --frozen bash ./zensical-docs.sh build
    uv run --frozen python scripts/check_built_site.py
    uv run --frozen python scripts/check_vercel_redirects.py
  )
else
  printf 'uv not found; skipping docs build validation\n' >&2
fi
