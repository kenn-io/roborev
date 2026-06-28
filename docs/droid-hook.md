---
title: Factory Droid Agent Hook
description: Let Factory Droid sessions run roborev-fix mid-session by watching the agent boundary
---

The `--agent droid` profile for `roborev agent-hook` is an opt-in integration
with the Factory Droid harness hook system. roborev reviews your commits in the
background; the Droid profile watches the agent boundary and, once review work
has piled up, returns one instruction telling Droid to run the `roborev-fix`
skill before the session goes cold. It shares the same local state daemon, loop
prevention, and failed-review detection as the Codex and Claude Code agent
hooks.

!!! note
    This is different from [Review Hooks](/guides/hooks/), which run your own
    shell commands when a review completes. The Droid agent-hook profile plugs
    into Factory Droid's own hook system to steer the agent itself.

## The Loop

Agents are good at making progress. They are worse at remembering to come back
after a background reviewer finishes, especially when reviews happen out of band
like they do with roborev.

The Droid agent-hook profile closes that gap. It sits behind Factory Droid
hooks, counts what happened in the current session, checks roborev for failed
reviews, and returns one direct instruction when there is review work to fix:

```text
Run the roborev-fix skill to address the unresolved roborev findings, then continue.
```

That turns review into part of the agent's normal rhythm: write code, get
reviewed, fix the review, continue.

!!! note
    The default instruction names `roborev-fix` generically. Droid invokes the
    skill as `/roborev-fix` (Factory's slash-command convention). Override
    `instruction` (see [Configuration](#configuration)) if you prefer different
    wording.

## What It Watches

The Droid profile tracks three signals per session:

- **Turns.** `Stop` hooks, so long-running sessions get periodic review repair.
- **Commits.** `PostToolUse` hooks on the `Execute` tool (Droid's shell tool)
  that produce commits. A `PreToolUse` `Execute` hook seeds the per-commit
  baseline so the count stays accurate.
- **Failed reviews.** Open, non-closed roborev reviews with a failed verdict.

It resolves the repository from the agent's working directory, so
outside a git repository it returns `{}` and stays out of the way. Reminders
also depend on the roborev daemon reporting an open failed review, so a
repository roborev does not track never produces a reminder.

If the main roborev daemon is unavailable, the failed-review check is skipped.
Turn and commit counts still work through the local hook daemon, but they only
prompt the agent once roborev reports at least one open failed review.

Commit-producing `Execute` calls are counted by default, but commit-based
prompts stay off unless `commit_threshold` is set above `0`. Failed-review
counts are scoped to the current git branch. Older jobs without a stored branch
are included, matching `roborev fix` discovery.

## Quick Start

The reminder tells Droid to run the `roborev-fix` skill, so install roborev's
Droid skills first if you have not already:

```bash
roborev skills install
```

Then install the hook entries:

```bash
roborev agent-hook install --agent droid
```

By default this updates `~/.factory/hooks.json` (the user scope, applied to every
project), registering `PreToolUse`, `PostToolUse` (both matching the `Execute`
tool), and `Stop` hooks. Existing hooks are preserved, and repeated installs are
idempotent. Use `--dry-run` to report what would change without writing.

Project-scoped Factory hooks are intentionally not supported by roborev because
`.factory/hooks.json` is executable repo-local configuration. Do not commit
Factory hook commands to a repository; install the Droid hook in your user scope
instead.

When roborev is installed through a version manager such as mise,
`agent-hook install --agent droid` resolves the same stable roborev shim used
by `roborev init`. To pin the exact binary path baked into the hook command,
use `--binary`:

```bash
roborev agent-hook install --agent droid --binary ~/.local/share/mise/shims/roborev
```

Use `--command` only when you want to provide the full hook command yourself.
`--binary` and `--command` are mutually exclusive.

For declarative setups (Nix home-manager, dotfiles) where editing those files in
place is the wrong shape, print the JSON for your config system to consume:

```bash
roborev agent-hook dump --agent droid --scope user
```

## Runtime Model

Factory Droid invokes:

```bash
roborev agent-hook run --agent droid
```

`run` reads a hook payload on stdin, talks to a small local `roborev-agent-hook`
daemon, and emits the hook response JSON Droid expects. This daemon is shared
with the other agent-hook profiles (it serves Codex, Claude Code, and Droid from
one process) and stores only local session counters under:

```text
${ROBOREV_DATA_DIR:-~/.roborev}/agent-hook/
```

The main roborev daemon stays the source of truth for reviews and jobs. `run`
auto-starts the local daemon on demand, and it fails open: if the daemon cannot
be reached or started, it emits `{}` and logs the diagnostic to stderr so a hook
never blocks the agent. Malformed input or a missing `session_id` is treated as
an invalid harness call and returns a normal CLI error.

Because the daemon is shared, you can also manage it through
`roborev agent-hook daemon start|status|stop|restart`. Manual management is
rarely needed.

## Configuration

Set thresholds in the `[droid_hook]` section of your global config
(`~/.roborev/config.toml`):

```toml
[droid_hook]
turn_threshold = 5
commit_threshold = 0
failed_review_threshold = 4
instruction = "Run the roborev-fix skill to address the unresolved roborev findings, then continue."
```

| Trigger | Default | TOML key | `run` flag | Environment variable |
|---------|---------|----------|------------|----------------------|
| Stop hooks (turns) | `5` | `turn_threshold` | `--turn-threshold` | `ROBOREV_DROID_HOOK_TURN_THRESHOLD` |
| Commit-producing `Execute` calls | `0` | `commit_threshold` | `--commit-threshold` | `ROBOREV_DROID_HOOK_COMMIT_THRESHOLD` |
| Open failed reviews | `4` | `failed_review_threshold` | `--failed-review-threshold` | `ROBOREV_DROID_HOOK_FAILED_REVIEW_THRESHOLD` |
| Continuation instruction | `Run the roborev-fix skill...` | `instruction` | `--instruction` | `ROBOREV_DROID_HOOK_INSTRUCTION` |
| roborev daemon address | runtime discovery | | `--roborev-server` | `ROBOREV_DROID_HOOK_ROBOREV_ADDR` |

Set any threshold to `0` to disable that trigger. Values resolve in this order,
highest priority first:

```text
run flags > environment variables > [droid_hook] config > defaults
```

`ROBOREV_DROID_HOOK_ROBOREV_ADDR` and `ROBOREV_AGENT_HOOK_DAEMON_ADDR` are
operational overrides only and are not persisted in TOML.
`ROBOREV_AGENT_HOOK_DAEMON_ADDR` points `run` at a specific local hook daemon
address shared by every agent-hook profile.

## Inspecting Sessions

Inspect tracked session counters, including `remind_count` (the number of
`roborev-fix` reminders emitted), as JSON. Because the daemon is shared, this
shows sessions from every integration (Droid, Codex, Claude Code):

```bash
roborev agent-hook status
```

Reset counters when you want a session to start fresh:

```bash
roborev agent-hook reset <session-id>   # reset one session
roborev agent-hook reset --all          # reset every session
```
