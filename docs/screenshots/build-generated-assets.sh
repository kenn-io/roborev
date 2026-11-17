#!/usr/bin/env bash
# Build every file on the docs-generated-assets branch into OUTPUT_DIR/assets
# from an approved corpus, so a screenshot runner with only Bash, Git, Python
# and Docker can call `make docs-screenshots` with DOCS_SCREENSHOTS_OUTPUT set.
set -euo pipefail

output="${1:?usage: build-generated-assets.sh OUTPUT_DIR}"
corpus="${DOCS_SCREENSHOTS_CORPUS:?set DOCS_SCREENSHOTS_CORPUS to an approved corpus manifest.json}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
docs_root="$(cd "$script_dir/.." && pwd)"
generated="$docs_root/assets/generated"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
# Verify the corpus before building anything.
python3 "$script_dir/corpus.py" extract "$corpus" "$work/roborev-demo-data"

# A capture checkout has no tags, so label the build with the newest release,
# as a local run does.
version="$(git ls-remote --refs --sort=-v:refname --tags https://github.com/kenn-io/roborev.git 'v*' |
    sed 's#.*refs/tags/##' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1)"
[[ -n "$version" ]] || { echo "ERROR: no roborev release tag found" >&2; exit 1; }

rm -rf "$generated"
TMPDIR="$work" ROBOREV_SCREENSHOT_VERSION="$version" \
    ROBOREV_SCREENSHOT_NOW="${DOCS_SCREENSHOTS_DATE:-}" bash "$script_dir/screenshot-all.sh" --skip-data

mkdir -p "$output/assets"
while IFS= read -r asset; do
    [[ -f "$generated/$asset" ]] || { echo "ERROR: screenshot run did not write $asset" >&2; exit 1; }
    cp "$generated/$asset" "$output/assets/$asset"
done < "$script_dir/expected-assets.txt"
echo "Wrote $(find "$output/assets" -type f | wc -l) files to $output/assets"
