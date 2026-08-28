#!/usr/bin/env bash
# Fail when a committed OpenAPI contract introduces breaking changes for
# existing API clients relative to a base git ref. Complements api-check,
# which only proves the committed contract matches the code.
#
# Usage: scripts/openapi-breaking.sh [base-ref]   (default: origin/main)
set -euo pipefail

base_ref="${1:-origin/main}"
specs=(
    pkg/client/openapi.yaml
)

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

status=0
for spec in "${specs[@]}"; do
    if ! git cat-file -e "$base_ref:$spec" 2>/dev/null; then
        echo "openapi-breaking: skipping $spec (absent at $base_ref)"
        continue
    fi
    base_copy="$tmp/${spec//\//_}"
    git show "$base_ref:$spec" >"$base_copy"
    echo "openapi-breaking: checking $spec against $base_ref"
    go tool oasdiff breaking "$base_copy" "$spec" \
        --allow-external-refs=false --fail-on ERR || status=1
done
exit $status
