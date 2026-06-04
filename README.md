<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/roborev-dev/roborev-docs/main/public/logo-with-text-dark-bg.svg">
  <img alt="roborev" src="https://raw.githubusercontent.com/roborev-dev/roborev-docs/main/public/logo-with-text-light.svg">
</picture>

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Docs](https://img.shields.io/badge/Docs-roborev.io-blue)](https://roborev.io)

**[Documentation](https://roborev.io)** | **[Quick Start](https://roborev.io/quickstart/)** | **[Installation](https://roborev.io/installation/)**

Continuous code review for AI coding agents. roborev runs in the
background, reviews every commit as agents write code, and surfaces
issues in seconds -- before they compound. Pull code reviews into
your agentic loop while context is fresh.

![roborev TUI](https://raw.githubusercontent.com/roborev-dev/roborev-docs/main/public/tui-hero.svg)

## How It Works

1. Run `roborev init` to install a post-commit hook
2. Every commit triggers a background review -- agents write, roborev reads
3. View findings in the TUI, feed them to your agent, or let `roborev fix` handle it

## Quick Start

```bash
cd your-repo
roborev init          # Install post-commit hook
git commit -m "..."   # Reviews happen automatically
roborev tui           # View reviews in interactive UI
```

If roborev is managed by a version manager, `roborev init` tries to
install hooks with the stable shim/symlink. You can also choose the exact
binary path with `roborev init --binary ~/.local/share/mise/shims/roborev`.

![roborev review](https://raw.githubusercontent.com/roborev-dev/roborev-docs/main/public/tui-review.svg)

## Features

- **Background Reviews** - Every commit is reviewed automatically via
  git hooks. No workflow changes required.
- **Auto-Fix** - `roborev fix` feeds review findings to an agent that
  applies fixes and commits. `roborev refine` iterates until reviews pass.
- **Agent Hook** - Optional Codex and Claude Code harness hooks can prompt
  active sessions to run `$roborev-fix` when roborev has open failed reviews.
- **Code Analysis** - Built-in analysis types (duplication, complexity,
  refactoring, test fixtures, dead code, security) that agents can fix
  automatically.
- **Multi-Agent** - Works with Codex, Claude Code, Gemini, Copilot,
  OpenCode, Cursor, Kiro, Kilo, Droid, and Pi.
- **Runs Locally** - No hosted service or additional infrastructure.
  Reviews are orchestrated on your machine using the coding agents
  you already have configured.
- **Interactive TUI** - Real-time review queue with vim-style navigation.
- **Review Verification** - `roborev compact` verifies findings against
  current code, filters false positives, and consolidates related issues
  into a single review.
- **Extensible Hooks** - Run shell commands on review events. Built-in
  [beads](https://github.com/steveyegge/beads) integration creates trackable issues from
  review failures automatically.

## The Agentic Fix Loop

When reviews find issues, copy-and-paste the reviews into your
interactive agent sessions, or invoke the `roborev:fix` skills. You
can also address open reviews on the command line non-interactively
with `roborev fix`.

`roborev fix` shows the review findings to an agent, which applies
changes and commits. The new commit gets reviewed automatically,
closing the loop.

For Codex and Claude Code sessions, `roborev agent-hook install` can add an
optional harness hook that prompts the active session to invoke `$roborev-fix`
after configured turn, commit, or failed-review thresholds are met. The hook
uses a separate local `roborev-agent-hook` daemon for session counters; it does
not run inside the main roborev daemon.

For fully automated iteration (advanced feature), use `refine`:

```bash
roborev refine                  # Fix, re-review, repeat until passing
```

`refine` runs in an isolated worktree and loops: fix findings, wait for
re-review, fix again, until all reviews pass or `--max-iterations` is hit.

## Code Analysis

Run targeted analysis across your codebase and optionally auto-fix:

```bash
roborev analyze duplication ./...           # Find duplication
roborev analyze refactor --fix *.go         # Suggest and apply refactors
roborev analyze complexity --wait main.go   # Analyze and show results
roborev analyze test-fixtures *_test.go     # Find test helper opportunities
roborev analyze security ./...              # Find security risks in existing code
```

Available types: `test-fixtures`, `duplication`, `refactor`, `complexity`,
`api-design`, `dead-code`, `architecture`, `security`.

Analysis jobs appear in the review queue. Use `roborev fix <id>` to
apply findings later, or pass `--fix` to apply immediately.

## Installation

**Shell Script (macOS / Linux):**
```bash
curl -fsSL https://roborev.io/install.sh | bash
```

**Homebrew (macOS / Linux):**
```bash
brew install roborev-dev/tap/roborev
```

**Windows (PowerShell):**
```powershell
powershell -ExecutionPolicy ByPass -c "irm https://roborev.io/install.ps1 | iex"
```

**With Go:**
```bash
go install go.kenn.io/roborev/cmd/roborev@latest
```

## Developer Setup

This repo uses [`prek`](https://prek.j178.dev/) for local pre-commit checks.
The hook is a local system hook that runs `make lint`, so pre-commit can apply
`golangci-lint --fix` automatically instead of using the upstream
`golangci-lint` pre-commit repository. The hook is configured with
`always_run = true`, so it runs on every commit, not just commits that touch
Go files.

```bash
brew install prek     # or use your preferred prek install method
prek install          # install the local git hook
prek run --all-files  # run the configured checks manually
```

If the hook rewrites files, re-stage them and re-run `git commit`. Use
`make lint-ci` when you want a non-mutating lint check.

## Commands

| Command | Description |
|---------|-------------|
| `roborev init` | Initialize roborev in current repo |
| `roborev tui` | Interactive terminal UI |
| `roborev status` | Show daemon and queue status |
| `roborev review <sha>` | Queue a commit for review |
| `roborev review --branch` | Review all commits on current branch |
| `roborev review --dirty` | Review uncommitted changes |
| `roborev fix` | Fix open reviews (or specify job IDs) |
| `roborev refine` | Auto-fix loop: fix, re-review, repeat |
| `roborev analyze <type>` | Run code analysis with optional auto-fix |
| `roborev agent-hook install` | Install optional Codex/Claude agent harness hooks |
| `roborev compact` | Verify and consolidate open review findings |
| `roborev show [sha]` | Display review for commit |
| `roborev run "<task>"` | Execute a task with an AI agent |
| `roborev close <id>` | Close a review |
| `roborev skills install` | Install agent skills for Claude/Codex |

See [full command reference](https://roborev.io/commands/) for all options.

## Configuration

Create `.roborev.toml` in your repo:

```toml
agent = "claude-code"
snapshot_dir = ".roborev"
review_guidelines = """
Project-specific review instructions here.
"""
```

`snapshot_dir` must be repo-relative. `roborev init` ensures it is ignored in `.gitignore`; snapshot creation also adds a local `.git/info/exclude` fallback for existing checkouts whose ignore setup is stale.

See [configuration guide](https://roborev.io/configuration/) for all options.

### Environment Variables

| Variable | Description |
|----------|-------------|
| `ROBOREV_DATA_DIR` | Override default data directory (`~/.roborev`) |
| `ROBOREV_COLOR_MODE` | TUI color theme: `auto` (default), `dark`, `light`, `none` |
| `ROBOREV_SYNC_CURSOR_LOOKBACK` | PostgreSQL sync cursor overlap duration (default `5m`) |
| `ROBOREV_AGENT_HOOK_TURN_THRESHOLD` | Override agent-hook Stop threshold |
| `ROBOREV_AGENT_HOOK_COMMIT_THRESHOLD` | Override agent-hook commit threshold |
| `ROBOREV_AGENT_HOOK_FAILED_REVIEW_THRESHOLD` | Override agent-hook failed-review threshold |
| `NO_COLOR` | Set to any value to disable all color output ([no-color.org](https://no-color.org)) |

## Supported Agents

| Agent | Install |
|-------|---------|
| Codex | `npm install -g @openai/codex` |
| Claude Code | `npm install -g @anthropic-ai/claude-code` |
| Gemini | `curl -fsSL https://antigravity.google/cli/install.sh \| bash` (preferred Antigravity CLI) or `npm install -g @google/gemini-cli` |
| Copilot | `npm install -g @github/copilot` |
| OpenCode | `npm install -g opencode-ai@latest` ([anomalyco/opencode](https://github.com/anomalyco/opencode)) |
| Cursor | [cursor.com](https://www.cursor.com/) |
| Kiro | [kiro.dev](https://kiro.dev/) |
| Kilo | `npm install -g @kilocode/cli` |
| Droid | [factory.ai](https://factory.ai/) |
| Pi | [pi.dev](https://pi.dev/) |

roborev auto-detects installed agents.

### Routing Claude Code to a proxy (Ollama, LiteLLM, etc.)

The `claude-code` agent accepts a model spec of the form `<model>@<base_url>`.
When `<base_url>` starts with `http(s)://`, roborev points Claude Code at
that endpoint and pins all tier aliases (Opus/Sonnet/Haiku/subagent) to the
given model.

```toml
# .roborev.toml — local Ollama for reviews, real Anthropic for fixes
agent = "claude-code"
review_model = "glm-5.1:cloud@http://127.0.0.1:11434"
fix_model    = "sonnet"
```

Or via CLI: `roborev review --model 'glm-5.1:cloud@http://127.0.0.1:11434'`.

**Proxy auth.** Set `ROBOREV_CLAUDE_PROXY_TOKEN` to forward a bearer token
to the proxy as `ANTHROPIC_AUTH_TOKEN`. If unset, roborev sends a placeholder
(sufficient for gateways that don't check the header, such as Ollama).
roborev does *not* forward `ANTHROPIC_API_KEY` to proxy endpoints — that
would leak a real Anthropic credential to arbitrary third parties.

**URL restrictions.** Proxy URLs must not embed `user:pass@` credentials
(use `ROBOREV_CLAUDE_PROXY_TOKEN`); `http://` is only accepted for loopback
hosts (`127.0.0.1`, `::1`, `localhost`) so plaintext endpoints can't receive
tokens over the wire. Use `https://` for remote proxies. The full URL
(including any path or query string) is forwarded as-is to
`ANTHROPIC_BASE_URL`, so include the path your gateway expects (e.g.
LiteLLM may want a trailing `/v1`; Ollama wants no path).

**Environment behavior (breaking change in this release).** When the
`claude-code` agent runs, roborev always strips inherited `ANTHROPIC_API_KEY`,
`ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`,
`ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL`, and `CLAUDE_CODE_SUBAGENT_MODEL`
from the child environment. If you were previously routing Claude Code by
exporting these vars in your shell, switch to the `<model>@<base_url>` spec
instead. For native (non-proxy) mode, configure `ANTHROPIC_API_KEY` via
roborev's config (it is re-injected from roborev's stored key, not inherited
from the operator's shell).

## Telemetry

roborev sends limited anonymous telemetry to PostHog when the daemon starts
and once every 24 hours while the daemon remains running: `daemon_started`
and `daemon_active` with repo count, review count, sync enabled, CI
enabled, and auto-design enabled, plus `application=roborev`, version, OS/arch,
`$process_person_profile=false`, `$geoip_disable=true`, and an anonymous install
ID.
It does not send repo names, paths, remotes, prompts, review output, provider
tokens, usernames, or IP geolocation. Set `ROBOREV_TELEMETRY_ENABLED=0` to
disable it. `TELEMETRY_ENABLED=0` is also honored. Telemetry is always disabled
inside Go test processes, regardless of environment variables.

## Security Model

roborev delegates code review and fix tasks to AI coding agents that
have shell access. Review agents may execute read-only git and shell
commands to inspect diffs; fix agents run in isolated worktrees with
full tool access.

**roborev is designed for use with trusted codebases.** The review
prompt includes diff content and commit messages from the repository.
If you are reviewing untrusted code (e.g., open-source contributions
from unknown authors), run roborev inside a sandboxed environment
(container, VM, or similar) to limit the blast radius of any
prompt-injection attack that could cause an agent to execute
unintended commands.

## Documentation

Full documentation available at **[roborev.io](https://roborev.io)**:

- [Quick Start](https://roborev.io/quickstart/)
- [Installation](https://roborev.io/installation/)
- [Commands Reference](https://roborev.io/commands/)
- [Configuration](https://roborev.io/configuration/)
- [Auto-Fixing with Refine](https://roborev.io/guides/auto-fixing/)
- [Code Analysis and Assisted Refactoring](https://roborev.io/guides/assisted-refactoring/)
- [Hooks](https://roborev.io/guides/hooks/)
- [Agent Hook](docs/agent-hook.md)
- [Agent Skills](https://roborev.io/guides/agent-skills/)
- [PostgreSQL Sync](https://roborev.io/guides/postgres-sync/)

For local development in this repo, install hooks with `prek install` or run
`make install-hooks` as a thin wrapper around `prek install`.

## License

MIT
