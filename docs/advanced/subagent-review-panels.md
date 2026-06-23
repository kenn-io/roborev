---
title: Subagent Review Panels
description: Fan out one review target to multiple reviewers and synthesize one actionable result
---

Subagent review panels let one daemon review target run through several named reviewer specs, then produce one synthesis parent review. Use panels when a branch, PR, or risky change needs more than one review perspective without creating a pile of separate jobs for humans to track.

Panels are the system behind the 0.57 CI poller. The poller now creates panel runs for PR reviews. If you do not configure a named CI panel, the existing `agents`, `review_types`, and `[ci.reviews]` matrix is adapted into an implicit panel so old CI configs keep working.

## Quick Start

Define reusable subagents and panels in either `~/.roborev/config.toml` or `.roborev.toml`:

```toml
[review]
default_panel = "branch_final"
hook_review_panel = "quick"

[review.subagents.bug]
agent = "codex"
review_type = "default"
instructions = "Focus on correctness, regressions, edge cases, and missing tests."

[review.subagents.security]
agent = "claude-code"
review_type = "security"
reasoning = "thorough"
timeout = "10m"
instructions = "Focus on authn/authz, secret handling, injection, and unsafe file access."

[review.subagents.design]
agent = "codex"
review_type = "design"
allow_failure = true
timeout = "3m"
instructions = "Helpful when available, but do not block the panel if this reviewer flakes."

[review.panels.quick]
members = ["bug"]

[review.panels.branch_final]
members = ["bug", "security", "design"]
synthesis_agent = "codex"
synthesis_model = "gpt-5.5"
synthesis_backup_agent = "claude-code"
synthesis_backup_model = "claude-opus-4-8"
```

Run a panel explicitly:

```bash
roborev review --branch --panel branch_final
roborev review --dirty --panel quick
```

Force a normal single agent review, even when `default_panel` or `hook_review_panel` is configured:

```bash
roborev review --branch --panel none
```

Panels require the daemon. `roborev review --local --panel branch_final` prints a note and runs a single agent review because local mode does not use daemon side panel resolution.

## How Panels Work

A panel run creates one member job per configured reviewer and one synthesis parent job. The synthesis job is blocked until all members reach a terminal state. Normal job lists and `roborev list` show the synthesis parent as the actionable review. The TUI can expand that parent row to inspect individual reviewers.

The parent review is what you close, fix, cancel, rerun, and wait on. Member jobs are implementation details for the panel run. Rerunning the parent starts a fresh panel run; member rows cannot be rerun directly.

Synthesis avoids extra agent work when it can:

- If all successful members pass, the parent output is `No issues found.`
- If exactly one member produced output, that output can be passed through directly.
- If multiple members produced findings, a read only synthesis agent verifies, deduplicates, preserves file and line references, groups by severity, and writes one combined result.
- If no member succeeds, roborev records a durable all failed review instead of pretending the code passed.

When a panel uses `min_severity`, synthesis may still run for a single failed member so findings below the threshold can be filtered consistently.

## Selection Rules

| Selector | Applies To | Behavior |
|----------|------------|----------|
| `--panel <name>` | Manual daemon reviews | Runs the named panel. This takes priority over config defaults. |
| `--panel none` | Manual daemon reviews | Forces single agent review. |
| `[review] default_panel` | Foreground daemon reviews | Used when no `--panel` flag is set. |
| `[review] hook_review_panel` | Automatic post commit reviews | Used for hook sourced reviews. `default_panel` is not consulted for hook reviews. |
| `[ci] panel` | CI poller PR reviews | Runs the named panel instead of the implicit CI matrix. |

Panels apply to code review targets: single commits, ranges, branch reviews, and dirty working tree reviews. Stored prompt jobs, including `roborev run`, analysis tasks, `compact`, and `insights`, do not fan out into panels.

## Configuration Reference

### Review Table

| Key | Type | Description |
|-----|------|-------------|
| `default_panel` | string | Named panel for manual daemon reviews when `--panel` is not set. |
| `hook_review_panel` | string | Named panel for automatic post commit reviews. |
| `subagents` | table | Named reviewer specs referenced by panels. |
| `panels` | table | Named panel specs. |

Global and repo level review configs are merged. Repo level `default_panel` and `hook_review_panel` override global values. Repo level `subagents` and `panels` are merged with global maps by name, and repo entries override global entries with the same name.

### Subagents

```toml
[review.subagents.security]
agent = "claude-code"
model = "claude-opus-4-8"
provider = "anthropic"
reasoning = "thorough"
review_type = "security"
instructions = "Focus on authentication and authorization."
allow_failure = true
timeout = "3m"
```

| Key | Type | Description |
|-----|------|-------------|
| `agent` | string | Agent for this member. Empty means use the workflow default. |
| `model` | string | Model for this member. Empty means use the workflow model resolution. |
| `provider` | string | Provider for agents that support provider selection, currently used by Pi. |
| `reasoning` | string | Reasoning level for this member. Empty means use review reasoning. |
| `review_type` | string | `default`, `security`, or `design`. `review` and `general` are accepted as aliases for `default`. |
| `instructions` | string | Additional instructions appended only to this member prompt. |
| `allow_failure` | bool | When true, a failed or canceled member does not make an otherwise successful panel fail. |
| `timeout` | duration string | Per-member job timeout such as `90s`, `3m`, or `1h`. Empty uses repo/global `job_timeout_minutes`. |

The member workflow is chosen from `review_type`: `default` uses review workflow config, `security` uses security workflow config, and `design` uses design workflow config. If a member sets `agent` but omits `model`, roborev inherits only a workflow specific model. It does not pair that explicit agent with an unrelated generic `default_model`.

`allow_failure` is how you mark a flaky or best-effort reviewer. The member still runs and its findings are included when it succeeds, but a failed or canceled run is tolerated when at least one required member produced usable output. `allow_failure` and `timeout` are independent; use both for a reviewer that is useful when available but should not block the panel, such as a reviewer running on flaky external infrastructure. If every required reviewer also fails and no member produces review output, the panel still records a failed/unavailable review instead of passing.

### Panels

```toml
[review.panels.branch_final]
members = ["bug", "security", "design"]
synthesis_agent = "codex"
synthesis_model = "gpt-5.5"
synthesis_backup_agent = "claude-code"
synthesis_backup_model = "claude-opus-4-8"
```

| Key | Type | Description |
|-----|------|-------------|
| `members` | array | Ordered list of subagent names. Required and must not be empty. |
| `synthesis_agent` | string | Agent for synthesis. Empty means use fix workflow agent resolution. |
| `synthesis_model` | string | Model for synthesis. Empty means use fix workflow model resolution. |
| `synthesis_backup_agent` | string | Explicit backup agent for synthesis if the primary is unavailable or fails. |
| `synthesis_backup_model` | string | Explicit backup model for synthesis backup. |

Panel validation fails if `default_panel` or `hook_review_panel` names an undefined panel, a panel has no members, or a panel references an undefined subagent.

## CI Panels

Set `[ci] panel = "name"` to run a named panel for daemon CI poller reviews:

```toml
[ci]
enabled = true
repos = ["myorg/myrepo"]
panel = "ci"

[review.subagents.bug]
agent = "codex"
review_type = "default"

[review.subagents.security]
agent = "claude-code"
review_type = "security"

[review.panels.ci]
members = ["bug", "security"]
synthesis_agent = "codex"
```

For repo specific CI behavior, put the same `[ci] panel = "ci"` override and panel definitions in that repo's `.roborev.toml`. The CI poller loads `.roborev.toml` from the repo's default branch before resolving panels, so a PR cannot change its own CI reviewer panel by modifying `.roborev.toml` on the feature branch.

If `[ci] panel` is empty, the poller uses the compatible matrix settings:

- `agents`
- `review_types`
- `[ci.reviews]`

That matrix becomes an implicit panel. Synthesis still produces one PR comment, and one member output can pass through without an extra synthesis agent call.

CI panel posting has additional safety checks. Before posting or retrying, roborev verifies the PR is still open, the HEAD SHA is unchanged, and the repo identity still matches. Stale runs are retired without posting a misleading comment. See [GitHub Integration](/integrations/github/) for CI retry behavior and status checks.

## Viewing Panel Runs

The TUI shows a panel run as one synthesis parent row. Press `Space` or `Right` to expand the row and inspect member reviewers; press `Space` or `Left` to collapse it. The parent row shows progress while reviewers are running and a compact member summary after completion.

`roborev show` includes a reviewer summary above the synthesized output for panel parents:

```text
3 reviewers: bug P, security F, design -
```

`roborev show --json` adds a `panel` block for synthesis parents:

```json
{
  "panel": {
    "run_uuid": "run-uuid-1",
    "name": "branch_final",
    "synthesis_job_id": 99,
    "members": [
      {
        "job_id": 40,
        "name": "bug",
        "agent": "codex",
        "review_type": "default",
        "status": "done",
        "verdict": "P"
      }
    ]
  }
}
```

When token usage is available, panel parent cost in the TUI includes known member costs even while the panel is still running or while some members are unpriced. Once synthesis reports usage, the parent total also includes synthesis cost. Treat the displayed value as a lower bound whenever not every member has reported cost.

## Operational Notes

Panels consume normal worker capacity. A three member panel can run up to three member jobs concurrently if `max_workers` allows it; the synthesis parent runs after the members finish.

Synthesis is read only. It may inspect the reviewed checkout to verify member findings, but it does not run in agentic mode and must not edit files.

Panel member instructions are appended after the normal review prompt. They are best for focus areas and review boundaries, not for replacing the base roborev review format.
