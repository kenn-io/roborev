---
title: Installation
description: Install roborev on your system
---

## Homebrew (macOS / Linux)

Install roborev from [Homebrew core](https://formulae.brew.sh/formula/roborev):

```bash
brew install roborev
```

No extra tap is needed. The package includes the CLI, terminal UI, and browser
application. Homebrew manages [updates](#update).

Homebrew also supports [Linux](https://docs.brew.sh/Homebrew-on-Linux).

## Install Script

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

Building Roborev requires Go 1.27.3 or newer. The embedded browser application
also requires Bun 1.4.4.

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

For Homebrew installations:

```bash
brew upgrade roborev
```

Homebrew builds do not replace themselves with `roborev update`. Use your
package manager to upgrade other
[package-managed builds](/docs/development/#package-builds).

For installations from the install script or a direct binary download:

```bash
roborev update
```

This downloads and replaces the current binary with the latest release. Read the
[changelog](/docs/changelog/) before updating, or press `U` in the terminal
interface to read recent release notes. The browser application also has a
release-notes viewer in its header.

### Upgrading to 0.74.0

Check this item before updating from 0.73.x or earlier:

| If you use... | Upgrade action |
| --- | --- |
| Claude Code reviews, panel synthesis, or planning | Run `claude --help` and check that it lists `--tools`. If it does not, upgrade Claude Code first. roborev now passes `--tools` so that Claude loads only `Read`, `Glob`, and `Grep`, and older Claude Code versions reject the flag. See [Strict Claude Code Reviews](/docs/agents/#strict-claude-code-reviews). |

### Upgrading to 0.73.0

Check these items before updating from 0.72.x or earlier:

| If you use... | Upgrade action |
| --- | --- |
| A custom `[review.types.test-value]` definition | Rename or remove it in global and repository configuration before upgrading. `test-value` is now a built-in review type, so custom definitions cannot use that name. Update commands, panels, and other references if you rename it. See [Custom Review Types](/docs/advanced/custom-review-types/). |
| Source builds | Use Go 1.27.3 or newer and Bun 1.4.4 for the embedded browser application. The Go update includes the certificate distrust fix for [CVE-2026-111943](https://pkg.go.dev/vuln/GO-2026-6809). |
| Muse Code | Install Muse Code 1.4.10 or later and put `muse` on PATH, or set `muse_cmd`. For pay-as-you-go use, set `model = "muse-spark-1.3"`. See [Muse Code](/docs/agents/#muse-code). |

If you are upgrading from 0.71.x or earlier, also follow the
[0.72.0 database conversion steps](#upgrading-to-0720) below before starting the
new daemon.

### Upgrading to 0.72.0

Version 0.72.0 reduces stored prompt size and speeds up large review histories.
Its first startup converts the local database before accepting requests. Prepare
for that conversion before updating from 0.71.x or earlier:

1. Stop the daemon with `roborev daemon stop` and back up its data directory. If
    you use PostgreSQL sync, stop every daemon sharing the mirror and back up
    the mirror too. If a service manager runs the daemon, stop it through that
    manager so it stays stopped. Until step 4, do not commit in repositories
    with roborev hooks or run roborev commands other than these steps. They
    start the daemon automatically, and after step 3 that starts the conversion
    early.
1. Check free disk space. Conversion keeps the original data while writing
    compressed copies and a write-ahead log. Its final `VACUUM` can need up to
    twice the database file's size in free space. See
    [conversion and disk requirements](/docs/configuration/#stored-prompts-and-retention).
1. Upgrade through your usual installation method. If you use `roborev update`,
    add `--no-restart`; without it, the update starts the new daemon to repair
    git hooks. Upgrade every machine sharing a PostgreSQL mirror before
    restarting those daemons; the mirror moves to schema 24, which older
    clients refuse.
1. Start the daemon with `roborev daemon start` and allow the conversion to
    finish, or start it through your service manager. Large histories can take
    several minutes. Run `roborev doctor` afterward to check the installation.

**Downgrading is unsupported.** Do not run an older binary against the converted
database. Older releases without schema-version checks may try to use it.

Prompt and log cleanup stays off unless you configure `prompt_retention_days` or
`job_log_retention_days`. The
[retention guide](/docs/configuration/#stored-prompts-and-retention) explains
what each setting removes and how sync affects cleanup.

If you use ZCode, run `roborev agent-hook install --agent zcode` after upgrading
and restarting the daemon. See [ZCode reminders](/docs/agent-hook/#zcode).
Update bundled agent skills with `roborev skills install` to add the
[Superpowers and Kata workflow](/docs/integrations/goal-review/).

### Upgrading to 0.71.0

Check these items when updating from 0.70.x or earlier:

| If you use... | Upgrade action |
| --- | --- |
| The TUI release-notes shortcut | Press `U` instead of `u`. `u` now pages up. See [TUI keyboard commands](/docs/integrations/tui/#keyboard-commands). |
| `roborev log --db <path>` | Remove `--db`. To read logs from a daemon that uses a custom database, select that daemon with `--server`. See [Job Logs](/docs/commands/#job-logs). |
| `roborev legacy-reviews` scripts | Keep the daemon running. The commands now work through the daemon, and `--db` only checks that the selected daemon owns that database. See [legacy review migration](/docs/guides/reviewing-code/#review-storage-and-legacy-migration). |
| Scripts that pass `--server` | Start that daemon yourself. If it is unavailable, commands now return an error instead of starting a local daemon. |

### Upgrading to 0.70.0

Check these settings when updating from 0.69.x or earlier:

| If you use... | Upgrade action |
| --- | --- |
| TUI startup filters | Replace the top-level `auto_filter_repo` and `auto_filter_branch` settings with `filter_repo` and `filter_branch` under `[tui]`. Both filters now default to `true`. To keep an unfiltered view, set both to `false`. See [TUI filtering](/docs/integrations/tui/#filtering). |
| Semantic search | Under `[search.embeddings]`, replace `api_key_env = "NAME"` with `api_key = { env = "NAME" }`, then restart the daemon. Key strings and private key files are also supported. See [embedding credentials](/docs/search/#embedding-credentials). |
| Agent Hook reminders | Add project review guidance on the default branch if the repository has none. Global guidelines alone no longer enable reminders. See [the guidance requirement](/docs/agent-hook/#review-guidelines-are-required). |

Reviews and keyword search keep working if a configured embedding key is
missing. Local embedding servers that need no authentication can leave `api_key`
unset.

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
