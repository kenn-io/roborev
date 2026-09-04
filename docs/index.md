---
title: roborev documentation
description: Operating documentation for roborev, the continuous code review daemon for coding agents
---

# roborev documentation

roborev reviews every commit in the background with the coding agents you
already run, keeps the findings in a ledger until they are addressed, and feeds
them back into the agent session while context is fresh. This is the operating
documentation. For the product story, start at the
[overview](https://roborev.io/) and the [guide](https://roborev.io/guide/).

<p class="hero-actions">
  <a class="md-button md-button--primary" href="/docs/quickstart/">Quick Start</a>
  <a class="md-button" href="https://github.com/kenn-io/roborev">View on GitHub</a>
</p>

## Install

=== "macOS / Linux"

    ```bash
    curl -fsSL https://roborev.io/install.sh | bash
    ```

=== "Homebrew"

    ```bash
    brew install kenn-io/tap/roborev
    ```

=== "Windows"

    ```powershell
    powershell -ExecutionPolicy ByPass -c "irm https://roborev.io/install.ps1 | iex"
    ```

=== "Go"

    ```bash
    go install go.kenn.io/roborev/cmd/roborev@latest
    ```

See [Installation](installation.md) for upgrades, packages, and uninstall.

## Quickstart

```bash
cd your-repo
roborev init                  # post-commit hook, daemon, repo registration
roborev agent-hook install    # optional: wire coding-agent sessions and skills
# do some work, generate commits
roborev tui                   # browse reviews in the terminal
roborev ui                    # or in the native browser application
```

New here? Run `roborev quickstart` and point your coding agent at it: the
built-in guide inspects the repo and helps finish configuration.

## How roborev works

![How roborev works](/docs/assets/static/how-it-works.svg){ loading=eager }

- **Post-commit reviews.** A git hook reviews every commit in the background
    with any supported agent. See
    [Post-Commit Reviews](automation/post-commit-reviews.md).
- **Agent hook.** Open findings are delivered back into Claude Code, Codex,
    Copilot, Cursor, Droid, Gemini, Hermes, and Qwen sessions with exact review
    IDs. See [Agent Hook](agent-hook.md).
- **Fix and refine.** `/roborev-fix` addresses open findings from inside the
    agent session; `/roborev-refine` re-reviews and fixes a whole branch until
    every review passes. See [Agent Skills](guides/agent-skills.md) and
    [Auto-Fix with Refine](guides/auto-fixing.md).

## Documentation map

<div class="grid cards" markdown>

- **Start**

    [Quick Start](quickstart.md), [Installation](installation.md), and the
    [Changelog](changelog.md).

- **Automation**

    [Post-Commit Reviews](automation/post-commit-reviews.md),
    [Agent Hook](agent-hook.md), and [Review Event Hooks](guides/hooks.md) for
    notifications and issue filing.

- **Interfaces**

    The [CLI](commands.md), the [Terminal UI](integrations/tui.md), and the
    [Browser UI](web-ui.md) with analytics.

- **Configuration**

    [Configuration](configuration.md) for agents, models, guidelines, panels,
    experiments, and [Supported Agents](agents/index.md).

- **Pull requests**

    [GitHub Integration](integrations/github.md) and
    [GitLab Integration](integrations/gitlab.md) for CI review panels and bot
    comments.

- **Guides**

    [Reviewing Code](guides/reviewing-code.md),
    [Responding to Reviews](guides/responding-to-reviews.md),
    [Agent Skills](guides/agent-skills.md),
    [Code Analysis](guides/assisted-refactoring.md),
    [Auto-Fix with Refine](guides/auto-fixing.md), and
    [Repository Management](guides/repository-management.md).

- **Advanced**

    [Background Tasks](advanced/background-tasks.md),
    [Subagent Review Panels](advanced/subagent-review-panels.md),
    [Custom Review Types](advanced/custom-review-types.md),
    [Custom Tasks](advanced/custom-tasks.md), [ACP](advanced/acp.md),
    [PostgreSQL Sync](advanced/postgres-sync.md), and
    [Streaming](advanced/streaming.md).

- **Integrations and help**

    [Kata](integrations/kata.md), [Claude Chic](integrations/claudechic.md),
    [Troubleshooting](guides/troubleshooting.md), and
    [Development](development.md).

</div>

## Architecture

<img src="/docs/assets/static/architecture.svg" alt="roborev architecture diagram" class="diagram-center" />

- **Daemon**: HTTP server on port 7373 (auto-finds an available port if busy)
- **Workers**: pool of 4 (configurable) parallel review workers
- **Storage**: SQLite at `~/.roborev/reviews.db` with WAL mode
- **Config**: global at `~/.roborev/config.toml`, per-repo at `.roborev.toml`

## For LLMs

This site publishes source Markdown next to each rendered page. Prefer the `.md`
URL when reading or citing docs programmatically: `/docs/changelog.md` for
`/docs/changelog/`, `/docs/guides/reviewing-code.md` for
`/docs/guides/reviewing-code/`, and `/docs/index.md` for this page.
[llms.txt](https://roborev.io/llms.txt) indexes every page.
