---
title: roborev
description: Continuous code review for coding agents
---

# roborev

Continuous code review for coding agents. Review commits immediately, catch issues early, and fix them while context is fresh.

<p class="hero-actions">
  <a class="md-button md-button--primary" href="/quickstart/">Read Docs</a>
  <a class="md-button" href="https://github.com/kenn-io/roborev">View on GitHub</a>
</p>

## How roborev works

![How roborev works](/assets/static/how-it-works.svg){ loading=eager }

roborev reviews every commit in the background and feeds findings back to your
coding agent so the write -> review -> fix loop runs hands-off.

- **Post-commit reviews** - every commit is reviewed automatically, with any agent.
- **Agent hook** - nudges your CLI agent to fix findings mid-session.
- **Refine before you ship** - `/roborev-refine` re-reviews and fixes your whole branch until every review passes, catching bugs before the PR.

[Set up automation ->](automation/post-commit-reviews.md)

<figure class="hero-shot" data-lightbox>
  <img src="/assets/generated/tui-hero.svg" alt="roborev TUI queue view" loading="eager">
</figure>

<div class="agent-matrix">
  <a href="/agents/"><img src="/assets/static/agents/codex.svg" alt="Codex" data-agent="codex" width="1604" height="719" /></a>
  <a href="/agents/"><img src="/assets/static/agents/claude-code.svg" alt="Claude Code" data-agent="claude-code" width="696" height="95" /></a>
  <a href="/agents/"><img src="/assets/static/agents/gemini.svg" alt="Gemini" data-agent="gemini" width="344" height="127" /></a>
  <a href="/agents/"><img src="/assets/static/agents/opencode.svg" alt="OpenCode" data-agent="opencode" width="641" height="115" /></a>
  <a href="/agents/"><img src="/assets/static/agents/copilot.svg" alt="Copilot" data-agent="copilot" width="419" height="95" /></a>
  <a href="/agents/"><img src="/assets/static/agents/cursor.svg" alt="Cursor" data-agent="cursor" width="800" height="190" /></a>
  <a href="/agents/"><span class="agent-text">Droid</span></a>
  <a href="/agents/"><span class="agent-text">Kilo</span></a>
  <a href="/agents/"><span class="agent-text">Kiro</span></a>
  <a href="/agents/"><img src="/assets/static/agents/pi.svg" alt="Pi" data-agent="pi" width="800" height="800" /></a>
</div>

## Quick Start

```bash
curl -fsSL https://roborev.io/install.sh | bash
```

Then from within your git repositories:

```bash
roborev init          # Install post-commit hook
# do some work, generate commits
roborev tui           # Browse reviews in the terminal UI
```

For Windows, see the [installation guide](/installation/#quick-install-recommended).

## Why roborev?

AI coding agents write code fast, but they make mistakes. Most review feedback comes too late. The agent has moved on and context is lost. roborev changes this:

1. **Ask your agents to commit often**, ideally every turn of work
2. **roborev reviews** each commit in the background
3. **Bring review work back into the agent session** with [`agent-hook`](/agent-hook/) (`--agent droid` for Factory Droid) or check the TUI (`roborev tui`) as findings arrive
4. **Address findings** by letting the hook prompt the fix skill, copying reviews into your agent, using [`/roborev-fix`](/guides/agent-skills/), or running `roborev fix`

Every commit gets reviewed. Issues surface in seconds, not hours. Open reviews stay in the TUI queue until explicitly addressed and closed, so nothing falls through the cracks.

<div class="grid cards" markdown>

-   **Review Ledger**

    Every commit is reviewed automatically via git hooks. Reviews accumulate in a persistent queue that acts as a ledger: nothing is closed until explicitly addressed.

-   **Agent-Ready Feedback**

    Use `agent-hook` to prompt Codex, Claude Code, or Droid (`--agent droid`) to run the fix skill when review work piles up. You can also copy findings into your agent session, use `/roborev-fix`, or run `roborev fix` to apply fixes automatically.

-   **Code Analysis**

    Built-in analysis types (duplication, complexity, refactoring, test fixtures, dead code) that agents can address directly.

-   **Multi-Agent**

    Works with Codex, Claude Code, Gemini, Copilot, OpenCode, Cursor, Droid, Kilo, Kiro, and Pi. Auto-detects installed agents.

-   **Rich Markdown Display**

    Reviews render with full Markdown formatting: syntax-highlighted code blocks, headings, lists, and inline styles, right in your terminal.

-   **Runs Locally**

    No hosted service or additional infrastructure. Reviews are orchestrated on your machine using the coding agents you already have configured.

-   **Multi-Machine Sync**

    Bi-directionally sync reviews across machines via PostgreSQL.

</div>

## Architecture

<img src="/assets/static/architecture.svg" alt="roborev architecture diagram" class="diagram-center" />

- **Daemon**: HTTP server on port 7373 (auto-finds available port if busy)
- **Workers**: Pool of 4 (configurable) parallel review workers
- **Storage**: SQLite at `~/.roborev/reviews.db` with WAL mode
- **Config**: Global at `~/.roborev/config.toml`, per-repo at `.roborev.toml`

## Federated Multiplayer

<img src="/assets/static/federation.svg" alt="roborev federation diagram" class="diagram-center" />

Bi-directionally sync reviews across machines with a shared PostgreSQL database. Each daemon maintains its local SQLite for fast access while syncing changes to the central database.
