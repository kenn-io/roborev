---
title: Installation
description: Install roborev on your system
---


## Quick Install (Recommended)

The install script downloads the latest release binary for your platform:

=== "macOS / Linux"

    ```bash
    curl -fsSL https://roborev.io/install.sh | bash
    ```

    This installs to `~/.local/bin` by default.

=== "Windows"

    ```powershell
    powershell -ExecutionPolicy ByPass -c "irm https://roborev.io/install.ps1 | iex"
    ```

    This installs to `%USERPROFILE%\.roborev\bin` and adds it to your PATH. Both x64 and ARM64 are supported.

    The installer verifies SHA256 checksums by default. To customize installation:

    | Environment Variable | Description |
    |---------------------|-------------|
    | `ROBOREV_INSTALL_DIR` | Custom install directory (default: `%USERPROFILE%\.roborev\bin`) |
    | `ROBOREV_NO_MODIFY_PATH` | Set to skip adding install dir to PATH |
    | `ROBOREV_SKIP_CHECKSUM` | Set to skip checksum verification (not recommended) |

## Homebrew (macOS / Linux)

Install via Homebrew:

```bash
brew install kenn-io/tap/roborev
```

Or tap first, then install:

```bash
brew tap kenn-io/tap
brew install roborev
```

This also works on Linux with [Linuxbrew](https://docs.brew.sh/Homebrew-on-Linux).

## Linux Packages: DEB and RPM

Starting with 0.57.0, GitHub releases include `.deb` and `.rpm` packages for Linux `amd64` and `arm64`. Download the package for your architecture from the [GitHub Releases](https://github.com/kenn-io/roborev/releases) page, then install it locally:

```bash
# Debian / Ubuntu
sudo apt install ./roborev_<version>_linux_amd64.deb

# Fedora / RHEL
sudo dnf install ./roborev_<version>_linux_amd64.rpm
```

The packages install the `roborev` binary to `/usr/bin` and include user-level systemd units for the daemon.

## Go Install

If you have Go installed:

```bash
go install go.kenn.io/roborev/cmd/roborev@latest
```

Ensure `$GOPATH/bin` is in your PATH:

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
```

## Build from Source

```bash
git clone https://github.com/kenn-io/roborev
cd roborev
make install
```

The `make install` target builds with version information embedded (e.g., `v0.7.0-5-gabcdef`).

For quick iteration during development:

```bash
go install ./cmd/...
```

## Verify Installation

```bash
roborev version
```

## Update

Update to the latest version:

```bash
roborev update
```

This downloads and replaces the current binary with the latest release.

## Agent Requirements

roborev requires at least one AI agent CLI to be installed. See [Supported Agents](/agents/) for the full list, installation commands, and configuration options.
