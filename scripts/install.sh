#!/bin/bash
# roborev installer
# Usage: curl -fsSL https://raw.githubusercontent.com/roborev-dev/roborev/main/scripts/install.sh | bash

set -euo pipefail

REPO="roborev-dev/roborev"
BINARY_NAME="roborev"
ROBOREV_INSTALL_TMPDIR=""

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info() { echo -e "${GREEN}$1${NC}"; }
warn() { echo -e "${YELLOW}$1${NC}"; }
error() { echo -e "${RED}$1${NC}" >&2; exit 1; }

# Detect OS
detect_os() {
    case "$(uname -s)" in
        Darwin) echo "darwin" ;;
        Linux) echo "linux" ;;
        MINGW*|MSYS*|CYGWIN*) echo "windows" ;;
        *) error "Unsupported OS: $(uname -s)" ;;
    esac
}

# Detect architecture
detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        armv7*|armhf) echo "arm" ;;
        *) error "Unsupported architecture: $(uname -m)" ;;
    esac
}

# Find install directory
find_install_dir() {
    if [ -w "/usr/local/bin" ]; then
        echo "/usr/local/bin"
    elif [ -w "$HOME/.local/bin" ]; then
        mkdir -p "$HOME/.local/bin"
        echo "$HOME/.local/bin"
    else
        mkdir -p "$HOME/.local/bin"
        echo "$HOME/.local/bin"
    fi
}

# Download with curl or wget
download() {
    local url="$1"
    local output="$2"
    if command -v curl &>/dev/null; then
        curl -fsSL "$url" -o "$output"
    elif command -v wget &>/dev/null; then
        wget -q "$url" -O "$output"
    else
        error "Neither curl nor wget found"
    fi
}

# Get latest release version
get_latest_version() {
    # Use the HTML /releases/latest endpoint, which 302-redirects to
    # /releases/tag/<version>. Unlike api.github.com it is not rate-limited
    # at 60 req/hr per IP, so users behind shared NAT / VPN don't get 403.
    local url="https://github.com/${REPO}/releases/latest"
    local final_url=""
    if command -v curl &>/dev/null; then
        final_url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$url") || return 1
    elif command -v wget &>/dev/null; then
        final_url=$(wget --spider -S "$url" 2>&1 \
            | awk 'tolower($1)=="location:" {print $2}' \
            | tail -1 \
            | tr -d '\r\n') || return 1
    else
        return 1
    fi
    case "$final_url" in
        */releases/tag/*) echo "${final_url##*/releases/tag/}" ;;
        *) return 1 ;;
    esac
}

# Install from GitHub releases
install_from_release() {
    local os="$1"
    local arch="$2"
    local install_dir="$3"

    info "Fetching latest release..."
    local version
    if ! version=$(get_latest_version); then
        return 1
    fi
    if [ -z "$version" ]; then
        return 1
    fi

    info "Found version: $version"

    local platform="${os}_${arch}"
    local filename="roborev_${version#v}_${platform}.tar.gz"
    local binary="$BINARY_NAME"
    if [ "$os" = "windows" ]; then
        filename="roborev_${version#v}_${platform}.zip"
        binary="roborev.exe"
    fi
    local url="https://github.com/${REPO}/releases/download/${version}/${filename}"

    local tmpdir
    tmpdir=$(mktemp -d)
    ROBOREV_INSTALL_TMPDIR="$tmpdir"
    trap 'rm -rf "$ROBOREV_INSTALL_TMPDIR"' EXIT
    local archive_path="$tmpdir/release.tar.gz"
    if [ "$os" = "windows" ]; then
        archive_path="$tmpdir/release.zip"
    fi

    info "Downloading ${filename}..."
    if ! download "$url" "$archive_path"; then
        return 1
    fi

    info "Extracting..."
    if [ "$os" = "windows" ]; then
        if command -v unzip &>/dev/null; then
            if ! unzip -q "$archive_path" -d "$tmpdir"; then
                return 1
            fi
        elif command -v powershell.exe &>/dev/null; then
            if ! ROBOREV_ARCHIVE_PATH="$archive_path" ROBOREV_EXTRACT_DIR="$tmpdir" powershell.exe -NoProfile -Command "Expand-Archive -LiteralPath \$env:ROBOREV_ARCHIVE_PATH -DestinationPath \$env:ROBOREV_EXTRACT_DIR -Force"; then
                return 1
            fi
        elif command -v powershell &>/dev/null; then
            if ! ROBOREV_ARCHIVE_PATH="$archive_path" ROBOREV_EXTRACT_DIR="$tmpdir" powershell -NoProfile -Command "Expand-Archive -LiteralPath \$env:ROBOREV_ARCHIVE_PATH -DestinationPath \$env:ROBOREV_EXTRACT_DIR -Force"; then
                return 1
            fi
        else
            warn "Neither unzip nor PowerShell found for extracting ${filename}"
            return 1
        fi
    else
        if ! tar -xzf "$archive_path" -C "$tmpdir"; then
            return 1
        fi
    fi

    # Install binary
    if [ ! -f "$tmpdir/$binary" ]; then
        warn "Downloaded release did not contain ${binary}"
        return 1
    fi
    if [ -w "$install_dir" ]; then
        if ! mv "$tmpdir/$binary" "$install_dir/"; then
            return 1
        fi
    else
        if ! sudo mv "$tmpdir/$binary" "$install_dir/"; then
            return 1
        fi
    fi
    if ! chmod +x "$install_dir/$binary"; then
        return 1
    fi

    # macOS code signing
    if [ "$os" = "darwin" ] && [ -f "$install_dir/roborev" ]; then
        codesign -s - "$install_dir/roborev" 2>/dev/null || true
    fi

    return 0
}

# Install using go install
install_from_go() {
    local install_dir="$1"

    if ! command -v go &>/dev/null; then
        return 1
    fi

    info "Installing via 'go install'..."
    if ! go install "go.kenn.io/roborev/cmd/roborev@latest"; then
        return 1
    fi

    return 0
}

# Main
main() {
    info "Installing roborev..."
    echo

    local os
    local arch
    local install_dir
    os=$(detect_os)
    arch=$(detect_arch)
    install_dir=$(find_install_dir)

    info "Platform: ${os}/${arch}"
    info "Install directory: ${install_dir}"
    echo

    # Try release first, then go install
    if install_from_release "$os" "$arch" "$install_dir"; then
        info "Installed from GitHub release"
    elif install_from_go "$install_dir"; then
        info "Installed via go install"
        install_dir="$(go env GOPATH)/bin"
    else
        error "Installation failed. Install Go from https://go.dev and try again."
    fi

    echo
    info "Installation complete!"
    echo

    # Check PATH
    if ! echo "$PATH" | grep -q "$install_dir"; then
        warn "Add this to your shell profile:"
        echo "  export PATH=\"\$PATH:$install_dir\""
        echo
    fi

    echo "Get started:"
    echo "  cd your-repo"
    echo "  roborev init"
    echo
    echo "For AI agent skills (Claude Code, Codex):"
    echo "  roborev skills install"
}

main "$@"
