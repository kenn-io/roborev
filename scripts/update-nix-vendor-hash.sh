#!/usr/bin/env bash
set -euo pipefail

repo_root="${1:-$(git rev-parse --show-toplevel)}"
flake_file="$repo_root/flake.nix"
fake_hash="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

if [[ ! -f "$flake_file" ]]; then
    echo "flake.nix not found, skipping vendorHash update"
    exit 0
fi

if ! command -v nix >/dev/null 2>&1; then
    echo "nix is required to update flake.nix vendorHash" >&2
    exit 1
fi

hash_count=$(grep -c 'vendorHash = "sha256-' "$flake_file" || true)
if [[ "$hash_count" -ne 1 ]]; then
    echo "expected exactly one vendorHash in flake.nix, found $hash_count" >&2
    exit 1
fi

old_hash=$(sed -nE 's/.*vendorHash = "(sha256-[^"]+)".*/\1/p' "$flake_file")
if [[ -z "$old_hash" ]]; then
    echo "failed to read current vendorHash from flake.nix" >&2
    exit 1
fi

restore_fake_hash() {
    sed -i.bak "s|vendorHash = \"$fake_hash\";|vendorHash = \"$old_hash\";|" "$flake_file"
    rm -f "$flake_file.bak"
}

if grep -q "vendorHash = \"$fake_hash\";" "$flake_file"; then
    echo "flake.nix already contains the fake vendorHash sentinel" >&2
    exit 1
fi

sed -i.bak "s|vendorHash = \"sha256-[^\"]*\";|vendorHash = \"$fake_hash\";|" "$flake_file"
rm -f "$flake_file.bak"
trap 'if grep -q "vendorHash = \"$fake_hash\";" "$flake_file"; then restore_fake_hash; fi' EXIT

set +e
output=$(cd "$repo_root" && nix build '.#default' --no-link -L 2>&1)
status=$?
set -e

if [[ "$status" -eq 0 ]]; then
    echo "nix build succeeded with the fake vendorHash" >&2
    exit 1
fi

new_hash=$(sed -nE 's/.*got:[[:space:]]+(sha256-[A-Za-z0-9+/=]+).*/\1/p' <<<"$output" | head -n 1)
if [[ -z "$new_hash" ]]; then
    echo "failed to extract vendorHash from nix build output" >&2
    echo "$output" >&2
    exit 1
fi

sed -i.bak "s|vendorHash = \"$fake_hash\";|vendorHash = \"$new_hash\";|" "$flake_file"
rm -f "$flake_file.bak"
trap - EXIT

echo "updated vendorHash to $new_hash"
cd "$repo_root"
nix build '.#default' --no-link -L
