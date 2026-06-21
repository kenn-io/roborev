#!/usr/bin/env bash
# Populate ignored docs asset directories from orphan asset branches.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$(cd "$script_dir/.." && pwd)"
repo_root="$(cd "$docs_root/.." && pwd)"
static_branch="${ROBOREV_DOCS_ASSETS_BRANCH:-docs-assets}"
generated_branch="${ROBOREV_DOCS_GENERATED_ASSETS_BRANCH:-docs-generated-assets}"
use_local_branches="${ROBOREV_DOCS_USE_LOCAL_ASSET_BRANCHES:-false}"

static_target="$docs_root/assets/static"
generated_target="$docs_root/assets/generated"

static_assets=(
  "agent-hook-feedback-loop.png"
  "agents/claude-code.svg"
  "agents/codex.svg"
  "agents/copilot.svg"
  "agents/cursor.svg"
  "agents/gemini.svg"
  "agents/opencode.svg"
  "agents/pi.svg"
  "architecture.svg"
  "claudechic-review-sidebar.png"
  "favicon-32.png"
  "favicon-64.png"
  "favicon.svg"
  "federation.svg"
  "logo-with-text-dark-bg.png"
  "logo-with-text-dark-bg.svg"
  "logo-with-text-light.png"
  "logo-with-text-light.svg"
  "logo-with-text.png"
  "logo-with-text.svg"
  "logo.png"
  "logo.svg"
  "og-image.png"
  "og-image.svg"
)

generated_assets=(
  "tui-hero.svg"
  "tui-queue.svg"
  "tui-review.svg"
  "tui-copy.svg"
  "tui-respond.svg"
  "tui-help.svg"
  "tui-address.svg"
  "cli-help.svg"
  "cli-repo-list.svg"
  "cli-status.svg"
)

has_expected_assets() {
  local target="$1"
  shift

  local asset
  for asset in "$@"; do
    [[ -f "$target/$asset" ]] || return 1
  done
}

in_git_worktree() {
  git -C "$repo_root" rev-parse --is-inside-work-tree >/dev/null 2>&1
}

resolve_asset_ref() {
  local branch="$1"

  if [[ "$use_local_branches" == "1" || "$use_local_branches" == "true" ]]; then
    if git -C "$repo_root" rev-parse --verify --quiet "$branch" >/dev/null; then
      printf '%s\n' "$branch"
      return 0
    fi
  fi

  if ! git -C "$repo_root" fetch --force --depth=1 origin \
    "+refs/heads/$branch:refs/remotes/origin/$branch" >/dev/null; then
    printf 'docs assets not hydrated: failed to fetch origin/%s\n' "$branch" >&2
    return 1
  fi

  if git -C "$repo_root" rev-parse --verify --quiet "origin/$branch" >/dev/null; then
    printf 'origin/%s\n' "$branch"
    return 0
  fi

  if [[ "$use_local_branches" == "1" || "$use_local_branches" == "true" ]] &&
    git -C "$repo_root" rev-parse --verify --quiet "$branch" >/dev/null; then
    printf '%s\n' "$branch"
    return 0
  fi

  return 1
}

hydrate_branch() {
  local branch="$1"
  local target="$2"
  shift 2

  if ! in_git_worktree; then
    if has_expected_assets "$target" "$@"; then
      return 0
    fi

    printf 'docs assets not hydrated: no git worktree found and expected assets are missing\n' >&2
    return 1
  fi

  local asset_ref
  if ! asset_ref="$(resolve_asset_ref "$branch")"; then
    printf 'docs assets not hydrated: %s branch unavailable\n' "$branch" >&2
    return 1
  fi

  rm -rf "$target"
  mkdir -p "$target"
  git -C "$repo_root" archive "$asset_ref" | tar -xf - -C "$target"

  if ! has_expected_assets "$target" "$@"; then
    printf 'docs assets not hydrated: %s is missing expected assets\n' "$branch" >&2
    return 1
  fi
}

hydrate_branch "$static_branch" "$static_target" "${static_assets[@]}"
hydrate_branch "$generated_branch" "$generated_target" "${generated_assets[@]}"
