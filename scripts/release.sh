#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

VERSION="$1"
EXTRA_INSTRUCTIONS="$2"

if [ -z "$VERSION" ]; then
    echo "Usage: $0 <version> [extra_instructions]"
    echo "Example: $0 0.2.0"
    echo "Example: $0 0.2.0 \"Focus on TUI improvements\""
    exit 1
fi

# Validate version format
if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Error: Version must be in format X.Y.Z (e.g., 0.2.0)"
    exit 1
fi

TAG="v$VERSION"

# Check if tag already exists
if git rev-parse "$TAG" >/dev/null 2>&1; then
    echo "Error: Tag $TAG already exists"
    exit 1
fi

# Check for uncommitted changes
if ! git diff-index --quiet HEAD --; then
    echo "Error: You have uncommitted changes. Please commit or stash them first."
    exit 1
fi

# Check if gh CLI is available (needed for PR creation)
if ! command -v gh &> /dev/null; then
    echo "Error: gh CLI is required for creating PRs. Install from https://cli.github.com/"
    exit 1
fi

# Set a top-level JSON string field in place using jq.
# Usage: set_json_field <file> <jq_path> <value>
set_json_field() {
    local FILE="$1"
    local JQ_PATH="$2"
    local VALUE="$3"
    local TMP_FILE
    TMP_FILE=$(mktemp)
    if ! jq --arg v "$VALUE" "$JQ_PATH = \$v" "$FILE" > "$TMP_FILE"; then
        rm -f "$TMP_FILE"
        echo "Error: failed to update $JQ_PATH in $FILE" >&2
        exit 1
    fi
    mv "$TMP_FILE" "$FILE"
}

# Rewrite the version field in agent plugin manifests in place (no commit).
# Populates PLUGIN_MANIFESTS with the list of manifests touched.
update_plugin_manifests_inplace() {
    local CLAUDE_PLUGIN="$REPO_ROOT/.claude-plugin/plugin.json"
    local CLAUDE_MARKETPLACE="$REPO_ROOT/.claude-plugin/marketplace.json"
    local CODEX_PLUGIN="$REPO_ROOT/.codex-plugin/plugin.json"

    PLUGIN_MANIFESTS=()
    [ -f "$CLAUDE_PLUGIN" ] && PLUGIN_MANIFESTS+=("$CLAUDE_PLUGIN")
    [ -f "$CLAUDE_MARKETPLACE" ] && PLUGIN_MANIFESTS+=("$CLAUDE_MARKETPLACE")
    [ -f "$CODEX_PLUGIN" ] && PLUGIN_MANIFESTS+=("$CODEX_PLUGIN")

    if [ ${#PLUGIN_MANIFESTS[@]} -eq 0 ]; then
        echo "No agent plugin manifests found, skipping update"
        return 0
    fi

    if ! command -v jq &> /dev/null; then
        echo "Error: jq is required to update agent plugin manifests." >&2
        echo "Install it from https://jqlang.github.io/jq/download/ and re-run." >&2
        exit 1
    fi

    echo "Updating agent plugin manifest versions to $VERSION..."
    [ -f "$CLAUDE_PLUGIN" ] && set_json_field "$CLAUDE_PLUGIN" '.version' "$VERSION"
    [ -f "$CODEX_PLUGIN" ] && set_json_field "$CODEX_PLUGIN" '.version' "$VERSION"
    [ -f "$CLAUDE_MARKETPLACE" ] && set_json_field "$CLAUDE_MARKETPLACE" '.plugins[0].version' "$VERSION"
}

# Commit any plugin manifest edits from update_plugin_manifests_inplace to the current branch.
# Sets PLUGIN_MANIFESTS_COMMITTED=1 when a commit is made.
commit_plugin_manifests() {
    if [ ${#PLUGIN_MANIFESTS[@]} -eq 0 ]; then
        return 0
    fi

    git -C "$REPO_ROOT" add -- "${PLUGIN_MANIFESTS[@]}"
    if git -C "$REPO_ROOT" diff --cached --quiet -- "${PLUGIN_MANIFESTS[@]}"; then
        echo "Agent plugin manifests already at version $VERSION, no changes needed"
        return 0
    fi
    git -C "$REPO_ROOT" commit -m "Update agent plugin manifests for $TAG" -- "${PLUGIN_MANIFESTS[@]}"
    PLUGIN_MANIFESTS_COMMITTED=1
}

# Update nix flake version and vendorHash, creating a PR if changes are needed
update_nix_flake() {
    local FLAKE_FILE="$REPO_ROOT/flake.nix"
    local BRANCH_NAME="release/$TAG-nix-update"

    if [ ! -f "$FLAKE_FILE" ]; then
        echo "Warning: flake.nix not found, skipping nix update"
        return 0
    fi

    # Save current ref to return to later (handles detached HEAD)
    local ORIGINAL_REF
    ORIGINAL_REF=$(git -C "$REPO_ROOT" symbolic-ref --short -q HEAD 2>/dev/null) || \
        ORIGINAL_REF=$(git -C "$REPO_ROOT" rev-parse HEAD)

    echo "Updating flake.nix version to $VERSION..."
    sed -i.bak "s/version = \"[^\"]*\"/version = \"$VERSION\"/" "$FLAKE_FILE"
    rm -f "$FLAKE_FILE.bak"

    # Check if vendorHash needs updating (only if go.mod changed since last release)
    if command -v nix &> /dev/null; then
        echo "Checking if vendorHash needs updating..."

        # Temporarily set vendorHash to empty to get the correct hash
        local OLD_HASH=$(grep 'vendorHash = "' "$FLAKE_FILE" | sed 's/.*vendorHash = "\([^"]*\)".*/\1/')
        sed -i.bak 's/vendorHash = "[^"]*"/vendorHash = ""/' "$FLAKE_FILE"

        # Try to build and capture the expected hash
        echo "Running nix build to compute vendorHash (this may take a moment)..."
        local NIX_OUTPUT
        if NIX_OUTPUT=$(nix build "$REPO_ROOT" 2>&1); then
            # Build succeeded with empty hash - dependencies might be empty or cached
            echo "Build succeeded, keeping existing vendorHash"
            sed -i.bak "s|vendorHash = \"\"|vendorHash = \"$OLD_HASH\"|" "$FLAKE_FILE"
        else
            # Extract the expected hash from the error message
            local NEW_HASH=$(echo "$NIX_OUTPUT" | grep -o 'sha256-[A-Za-z0-9+/=]*' | tail -1)
            if [ -n "$NEW_HASH" ]; then
                echo "Updating vendorHash to $NEW_HASH"
                sed -i.bak "s|vendorHash = \"\"|vendorHash = \"$NEW_HASH\"|" "$FLAKE_FILE"
            else
                echo "Warning: Could not determine new vendorHash, restoring old value"
                sed -i.bak "s|vendorHash = \"\"|vendorHash = \"$OLD_HASH\"|" "$FLAKE_FILE"
            fi
        fi
        rm -f "$FLAKE_FILE.bak"

        # Verify the build works
        echo "Verifying nix build..."
        if ! nix build "$REPO_ROOT" 2>/dev/null; then
            echo "Error: nix build failed after updating flake.nix"
            echo "Please fix flake.nix manually and try again"
            git -C "$REPO_ROOT" checkout -- flake.nix
            exit 1
        fi
        echo "Nix build successful!"
    else
        echo "Warning: nix not installed, cannot verify vendorHash"
        echo "If go.mod changed, you may need to update vendorHash manually"
    fi

    # Create PR for flake.nix changes if any
    if ! git -C "$REPO_ROOT" diff --quiet -- flake.nix; then
        echo "Creating PR for flake.nix updates..."

        # Ensure we return to original ref even on failure
        cleanup_branch() {
            git -C "$REPO_ROOT" checkout "$ORIGINAL_REF" 2>/dev/null || true
        }
        trap cleanup_branch EXIT

        # Create/reset branch for the PR (-B forces creation even if exists)
        git -C "$REPO_ROOT" checkout -B "$BRANCH_NAME"
        git -C "$REPO_ROOT" add flake.nix
        # Only commit if there are staged changes (handles retry case)
        if ! git -C "$REPO_ROOT" diff --cached --quiet; then
            git -C "$REPO_ROOT" commit -m "Update flake.nix for $TAG"
        fi
        git -C "$REPO_ROOT" push -u origin "$BRANCH_NAME" --force-with-lease

        # Create the PR (skip if an open PR already exists)
        if [ -n "$(gh pr list --state open --head "$BRANCH_NAME" --json number --jq '.[0].number' 2>/dev/null)" ]; then
            echo "Open PR for $BRANCH_NAME already exists, skipping creation"
        else
            gh pr create \
                --title "Update flake.nix for $TAG" \
                --body "Updates flake.nix version to $VERSION for the $TAG release." \
                --base main
            echo "PR created for flake.nix updates"
        fi

        # Return to original ref and clear trap
        trap - EXIT
        git -C "$REPO_ROOT" checkout "$ORIGINAL_REF"
    else
        echo "No flake.nix changes needed"
    fi
}

# Update nix flake before creating the release
update_nix_flake

# Create a temp file for the changelog
CHANGELOG_FILE=$(mktemp)
trap 'rm -f "$CHANGELOG_FILE"' EXIT

# Use changelog.sh to generate the changelog
"$SCRIPT_DIR/changelog.sh" "$VERSION" "-" "$EXTRA_INSTRUCTIONS" > "$CHANGELOG_FILE"

echo ""
echo "=========================================="
echo "PROPOSED CHANGELOG FOR $TAG"
echo "=========================================="
cat "$CHANGELOG_FILE"
echo ""
echo "=========================================="
echo ""

# Ask for confirmation
read -p "Accept this changelog and create release $TAG? [y/N] " -n 1 -r
echo ""

if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Release cancelled."
    exit 0
fi

# Update and commit agent plugin manifest version bumps so the tag points at a
# commit with the new versions. Run only after confirmation so that earlier
# failures (changelog gen, interrupt, etc.) cannot leave the tree dirty.
PLUGIN_MANIFESTS=()
PLUGIN_MANIFESTS_COMMITTED=0
update_plugin_manifests_inplace
commit_plugin_manifests

# Create the tag with changelog as message
echo "Creating tag $TAG..."
git tag -a "$TAG" -m "Release $VERSION

$(cat $CHANGELOG_FILE)"

if [ "$PLUGIN_MANIFESTS_COMMITTED" = "1" ]; then
    echo "Pushing branch to origin..."
    git push origin HEAD
fi

echo "Pushing tag to origin..."
git push origin "$TAG"

echo ""
echo "Release $TAG created and pushed successfully!"
echo "GitHub Actions will create the release with the changelog from the tag message."
echo ""
echo "GitHub release URL: https://github.com/roborev-dev/roborev/releases/tag/$TAG"
