---
title: Reviewing Code
description: Review branches, uncommitted changes, and commit ranges
---

## Feature Branches

Use `--branch` to review all commits since your branch diverged from main:

```bash
roborev review --branch              # Review branch vs auto-detected main/master
roborev review --branch=feature-xyz  # Review a specific branch by name
roborev review --branch --base dev   # Review branch vs specific base
```

Reviews are enqueued and run in the background. Open `roborev tui` to browse results as they complete. This is the recommended workflow for pre-merge reviews of entire feature branches.

### Reviewing a Different Branch

By default `--branch` reviews the current branch. You can specify a branch name to review a different branch without switching to it:

```bash
roborev review --branch=feature-xyz
roborev review --branch=feature-xyz --base develop
```

Note: use `--branch=name` (with `=`), not `--branch name`. The space-separated form treats `name` as a positional commit argument.

### How It Works

1. roborev detects the merge-base between the target branch and the base branch
2. The merge-base to branch tip range is queued as one review target
3. The AI agent reviews the branch range with per-commit context
4. Results are stored and can be viewed in the TUI

### Pre-Merge Review

Before creating a pull request, review your entire branch:

```bash
git checkout feature-branch
roborev review --branch          # Enqueue branch review (runs in background)
roborev tui                      # Browse results as they arrive
roborev compact                  # Consolidate findings across commits
roborev fix                      # Address consolidated findings
```

The TUI gives you a persistent queue of open reviews that must be explicitly addressed and closed, creating an accountability loop that prevents findings from getting lost.

### Panel Reviews

For high-value branch reviews, run a named panel so multiple reviewers check the same range and roborev synthesizes one parent review:

```bash
roborev review --branch --panel branch_final
```

If your config sets `[review] default_panel`, manual daemon reviews use it automatically. Use `--panel none` to force a normal single-agent review:

```bash
roborev review --branch --panel none
```

See [Subagent Review Panels](/advanced/subagent-review-panels/) for panel configuration.

Panel subagents can be marked `allow_failure = true` when a reviewer is useful but flaky. Those members still contribute findings when they succeed, but a transient failure or cancellation does not fail the synthesized parent if required reviewers produced usable output.

### CI Integration

```bash
if ! roborev review --branch --wait --quiet; then
    echo "Reviews found issues"
    exit 1
fi
```

### Branch Review Options

| Flag | Description |
|------|-------------|
| `--branch [name]` | Review all commits on the branch since base. Optionally specify a branch name with `--branch=name`. |
| `--base <branch>` | Compare against a specific base branch (default: auto-detect main/master) |
| `--wait` | Block until the review or panel parent completes (for CI and scripting; see [When to use `--wait`](#when-to-use-wait)) |
| `--quiet` | Suppress output |
| `--agent <name>` | Use specific agent |
| `--reasoning <level>` | Set reasoning depth |
| `--min-severity <level>` | Only report findings at or above this severity (`low`/`medium`/`high`/`critical`) |
| `--panel <name or none>` | Run a named review panel, or use `none` to force single-agent review |

## Uncommitted Changes

Use `--dirty` to review working tree changes before committing:

```bash
roborev review --dirty           # Queue review of uncommitted changes
```

Results appear in `roborev tui` once the review completes.

### What Gets Reviewed

The `--dirty` flag includes:

- Staged changes
- Unstaged changes to tracked files
- Untracked files

### Dirty Review Options

| Flag | Description |
|------|-------------|
| `--wait` | Block until review completes (for CI and scripting; see [When to use `--wait`](#when-to-use-wait)) |
| `--quiet` | Suppress output |
| `--agent <name>` | Use specific agent |
| `--reasoning <level>` | Set reasoning depth |
| `--min-severity <level>` | Only report findings at or above this severity (`low`/`medium`/`high`/`critical`) |
| `--panel <name or none>` | Run a named review panel, or use `none` to force single-agent review |

## Review Types

Use `--type` to change what the reviewer focuses on. Omitting `--type` gives you the standard code review.

```bash
roborev review                       # Default code review
roborev review --type security       # Security-focused review
roborev review --type design         # Design-focused review
roborev review --type lookahead      # Time-series look-ahead bias review
roborev review --branch --type security  # Security review of branch
```

| Type | Focus |
|------|-------|
| *(default)* | Bugs, security, testing gaps, regressions, code quality. This is what you get when you omit `--type`. |
| `security` | Injection, auth, credential exposure, path traversal, unsafe patterns |
| `design` | Completeness, feasibility, task scoping, missing considerations |
| `lookahead` | Time-series look-ahead bias (a.k.a. peekahead / future-data leakage): window/lag direction, train/test ordering, fit-on-full-series, as-of join alignment, point-in-time correctness, label/target leakage |

Review types work with all review modes (`--branch`, `--dirty`, `--since`, single commits, ranges).

The `security` and `design` types can have their own agent and model configuration via `{type}_agent` and `{type}_model` in `.roborev.toml` or global config. See [Workflow-Specific Agent and Model](/configuration/#workflow-specific-agent-and-model). `lookahead` has no dedicated fields, so pin it through the generic per-type block:

```toml
[analyze.lookahead]
agent = "claude-code"
model = "sonnet"
```

When unset, it falls back to your repo or global default agent and model.

## Task Context from Kata

If your repo is bound to a [Kata](https://github.com/kenn-io/kata) project, reviews can include Kata task context in the prompt:

```toml
[kata_context]
mode = "current"   # off (default), current, or open
max_chars = 50000
```

`current` mode includes only Kata issues referenced in the reviewed commit messages, such as `Closes: kata#abc4`. `open` mode includes the open Kata backlog as background context. Dirty reviews have no commit messages, so `current` mode contributes no Kata context there.

Kata context is optional and applies to local reviews only; CI pull-request reviews never include it. If the `kata` CLI is missing or the repo is not bound to Kata, the review proceeds without it. See [Kata Integration](/configuration/#kata-integration) for setup and [Built-in: Kata Integration](/guides/hooks/#built-in-kata-integration) for filing review findings back into Kata.

## Severity Filtering

Use `--min-severity` to limit which findings the reviewer reports:

```bash
roborev review --min-severity high          # Only report high and critical findings
roborev review --branch --min-severity medium  # Skip low-severity findings in branch reviews
```

The severity filter is injected into the review prompt as an instruction. Findings below the threshold are omitted from the review output. When `min_severity` is set and all findings fall below the threshold, the review receives a passing verdict.

Set a default per repo in `.roborev.toml` or globally in `~/.roborev/config.toml`:

```toml
review_min_severity = "medium"
```

The cascade order is: CLI flag > per-repo config > global config. The CLI flag overrides the config value. See [Configuration](/configuration/#per-repository-options).

## Review Guidelines

Use `review_guidelines` to give reviewers durable context about your project or your whole machine:

```toml
# ~/.roborev/config.toml
review_guidelines = """
Prefer concrete, actionable findings.
Do not flag localhost-only data paths as remote trust boundaries.
"""

# .roborev.toml
review_guidelines = """
This repo has no production database yet.
Public APIs must keep backward-compatible JSON fields.
"""
```

Global guidelines apply to every repo. Repo guidelines are appended after global guidelines by default, so a repo can add local rules without losing shared preferences. Set `review_guidelines_supersede_global = true` in `.roborev.toml` when a repo should replace the global text entirely.

See [Review Guidelines](/configuration/#review-guidelines) for common patterns and precedence details.

## Specific Commit Ranges

Use `--since` to review commits since a specific point:

```bash
roborev review --since HEAD~5       # Review last 5 commits
roborev review --since abc123       # Review commits since abc123 (exclusive)
roborev review --since v1.0.0       # Review commits since a tag
```

The range is exclusive of the starting commit (like git's `..` range syntax). Unlike `--branch`, this works on any branch including main.

## Large Diffs

For `--dirty` reviews, diffs are limited to 200KB since uncommitted changes cannot be easily inspected by the agent. If your dirty diff exceeds this limit, commit your changes in smaller chunks.

For committed changes, diffs over 250KB are omitted from the prompt - the agent is given only the commit hash and can inspect changes using `git show`.

## Session Reuse

!!! warning "Experimental"
    Session reuse is experimental. The behavior and configuration may change in future releases. Enable it on a per-repo basis first to evaluate before rolling out globally.

When reviewing a branch with multiple commits, each review normally starts a fresh agent session. This means the agent re-reads the repository context, re-analyzes unchanged files, and rebuilds its understanding from scratch for every commit. Session reuse changes this: when enabled, the daemon looks for a completed review on the same branch that used the same agent and review type, and resumes that session instead of starting a new one.

The result is that the agent retains its prior conversation context (file contents it already read, architectural understanding it built up, earlier findings it made) and only needs to analyze what changed since the last review. This can substantially reduce token usage and review latency on active branches with frequent commits.

### Enabling Session Reuse

Set `reuse_review_session = true` in your config:

```toml
# Per repo (.roborev.toml)
reuse_review_session = true

# Or globally (~/.roborev/config.toml)
reuse_review_session = true
```

### How It Works

When a new review is enqueued, the daemon searches for a prior completed review on the same branch that matches the repo, agent, and review type. Candidates are checked newest-first and must pass two safety checks:

1. **Ancestor check**: The candidate's reviewed commit must be an ancestor of the current target. This prevents reusing sessions from rebased or force-pushed branches where the history no longer applies.
2. **Distance limit**: The candidate must be within 50 commits of the current target. Sessions from much earlier in the branch history are too stale to provide useful context.

If a valid candidate is found, its session ID is passed to the agent, which resumes the prior conversation. If no candidate qualifies, the review starts fresh as usual.

### Supported Agents

Session reuse requires agent-side support for resuming conversations. The following agents support it:

| Agent | Resume mechanism |
|-------|-----------------|
| Codex | `codex exec resume --json <session>` |
| Claude Code | `claude --resume <session>` |
| OpenCode | `opencode run --session <session>` |
| Kilo | `kilo run --session <session>` |
| Pi | `pi --session <path>` |

Agents that do not support session reuse (Gemini, Copilot, Cursor, Kiro, Droid) ignore the setting and always start fresh sessions.

### Tuning with Lookback

By default, the daemon considers all prior sessions on the branch as candidates. On long-lived branches with many reviews, you can limit the search to the most recent N candidates:

```toml
reuse_review_session_lookback = 5   # Only consider 5 most recent sessions
```

This is rarely needed. The default (unlimited) works well because the ancestor and distance checks already filter out stale candidates.

## Waiting for a Review Without Enqueuing

When a post-commit hook already triggers `roborev review`, you don't
need `review --wait` (which enqueues a duplicate job). Use
`roborev wait` to block until the existing job completes:

```bash
roborev wait                     # Wait for most recent job for HEAD
roborev wait abc123              # Wait for job matching a specific commit
roborev wait --sha HEAD~1        # Explicit git ref
roborev wait --job 42            # Wait for a specific job ID
roborev wait --quiet             # Suppress output (exit code only)
```

Exit codes: 0 for PASS, 1 for FAIL or no job found.

### Argument Resolution

A positional argument is resolved as a git ref first (so numeric SHAs
like `123456` are handled correctly), then as a numeric job ID. Use
`--sha` or `--job` to disambiguate when needed.

### Agent Review-Fix Loops

`roborev wait` is designed for coding agents running review-fix
refinement loops. The agent commits, the hook enqueues the review,
and the agent calls `wait` to block until the verdict is ready. This
avoids wasting tokens on repeated `roborev list` or `roborev show`
invocations.

```bash
# Typical agent loop
git commit -m "Fix auth validation"   # Hook triggers review
roborev wait --quiet                  # Block until verdict
# Exit code 0 = pass, 1 = fail
```

## When to use `--wait`

By default, `roborev review` enqueues reviews and returns immediately. The `--wait` flag blocks until the review completes and prints the result to stdout.

This is a convenience for:

- **CI pipelines**: gate merges on review outcomes using the exit code
- **Orchestrators and one-shot agents**: scripts or automated systems that need a synchronous result before proceeding
- **`roborev refine`**: the iterative fix loop uses `--wait` internally to re-review after each fix

`--wait` is **not recommended inside interactive agent sessions** (Claude Code, Codex, etc.). Reviews requested with `--wait` still appear in the TUI, but in practice the result scrolls past in the conversation and is easy to lose track of. The async workflow creates a persistent accountability loop: reviews stay open in the TUI queue until explicitly addressed and closed, so nothing falls through the cracks.

### Exit Codes

When `--wait` is used, the exit code reflects the review verdict:
- Code 0 for passing reviews
- Code 1 for failing reviews

```bash
# CI example
if ! roborev review --branch --wait --quiet; then
    echo "Reviews found issues"
    exit 1
fi
```

## Addressing Findings

After reviewing, use `roborev fix` to let an agent address any failed reviews:

```bash
roborev fix                      # Fix open reviews on this branch
```

Browse open reviews first with `roborev tui`, then fix them. See [Responding to Reviews](/guides/responding-to-reviews/) for the full set of options.

## See Also

- [Responding to Reviews](/guides/responding-to-reviews/): Fix findings and add comments
- [Code Analysis & Refactoring](/guides/assisted-refactoring/): Targeted analysis with `roborev analyze`
- [Auto-Fix with Refine](/guides/auto-fixing/): Automated fix loop
- [Agent Skills](/guides/agent-skills/): Review and fix from within an agent session
