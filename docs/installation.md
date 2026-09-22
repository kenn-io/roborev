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

    This installs to `%USERPROFILE%\.roborev\bin` and adds it to your PATH. Both x64
    and ARM64 are supported.

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

This also works on Linux with
[Linuxbrew](https://docs.brew.sh/Homebrew-on-Linux).

The `kenn-io/tap` repository watches official Roborev releases and manages
formula updates from their published checksums. Roborev release jobs do not need
a cross-repository credential to update the formula.

## Linux Packages: DEB and RPM

Starting with 0.57.0, GitHub releases include `.deb` and `.rpm` packages for
Linux `amd64` and `arm64`. Download the package for your architecture from the
[GitHub Releases](https://github.com/kenn-io/roborev/releases) page, then
install it locally:

```bash
# Debian / Ubuntu
sudo apt install ./roborev_<version>_linux_amd64.deb

# Fedora / RHEL
sudo dnf install ./roborev_<version>_linux_amd64.rpm
```

The packages install the `roborev` binary to `/usr/bin` and include user-level
systemd units for the daemon.

## Go Install

If you have Go installed:

```bash
go install go.kenn.io/roborev/cmd/roborev@latest
```

Ensure `$GOPATH/bin` is in your PATH:

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
```

Go module source archives contain a compilation stub rather than generated web
assets, so `go install ...@latest` installs the CLI and terminal UI but leaves
the browser listener disabled. Use a release package or `make install` when you
need the browser application.

## Build from Source

Building Roborev requires Go 1.27.0 or newer. The embedded browser application
also requires Bun 1.3.14.

```bash
git clone https://github.com/kenn-io/roborev
cd roborev
make install
```

The `make install` target builds and validates the browser application and
embeds it alongside version information (for example, `v0.7.0-5-gabcdef`).

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

This downloads and replaces the current binary with the latest release. Read the
[changelog](/docs/changelog/) before updating, or press `u` in the terminal
interface to read recent release notes. The browser application also has a
release-notes viewer in its header.

### Upgrading to 0.68.1

Version 0.68.1 restores historical reviews archived by 0.68.0. Reviews that
cannot be converted faithfully remain usable as labeled legacy documents. For
recovery details and optional agent-driven conversion, see
[troubleshooting missing or unstructured reviews](/docs/guides/troubleshooting/#missing-or-unstructured-reviews-after-upgrade)
and the
[canonical migration guide](https://github.com/kenn-io/roborev/pull/1219#agent-migration-guide).

### Upgrading to 0.68.0

Check the rows that apply to your installation:

| If you use... | Upgrade action |
| --- | --- |
| Existing review history | Reviews that cannot be converted faithfully to JSON move to an archive and leave normal views. Follow [legacy review migration](/docs/guides/reviewing-code/#review-storage-and-legacy-migration) to export, convert, and restore them. |
| Pi reviews | Install the JSON schema extension described in [Pi Structured Output](/docs/agents/#pi-structured-output). It is required for reviews as well as classification. |
| Generated CI workflows | Regenerate them to adopt the credential changes. Copilot needs a separate `COPILOT_GITHUB_TOKEN`. See [the generated workflow](/docs/integrations/github/#how-the-generated-workflow-works). |
| An obsolete Agent Hook registration | Remove the command named by the error from your agent configuration, then reinstall. See [Agent Hook runtime](/docs/agent-hook/#runtime-model). |
| Git hooks in a working tree or external directory | Update them explicitly; automatic maintenance leaves them unchanged, including through symlinks. See [hook maintenance](/docs/guides/repository-management/#git-hook-maintenance). |
| The Nix flake | Roborev no longer ships it. Choose one of the installation methods above. |

## Agent Requirements

roborev requires at least one AI agent CLI to be installed. See
[Supported Agents](/docs/agents/) for the full list, installation commands, and
configuration options.
