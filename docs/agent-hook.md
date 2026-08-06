---
title: Agent Hook
description: Let Codex, Claude Code, and Factory Droid sessions run roborev-fix mid-session by watching the agent boundary
---

`roborev agent-hook` is an opt-in integration with the Codex, Claude Code, and
Factory Droid harness hook systems. roborev reviews your commits in the
background; `agent-hook` watches the agent boundary and, once review work has
piled up, returns one instruction telling the agent to run the fix skill before
the session goes cold. It is the in-tree replacement for the standalone
`roborev-hook` tool.

!!! note

    This is different from [Review Hooks](/guides/hooks/), which run your own shell
    commands when a review completes. Agent Hook plugs into the coding agent's own
    hook system to steer the agent itself.

<figure class="screenshot" data-lightbox>
  <img src="/assets/static/agent-hook-feedback-loop.png" alt="Agent hook asynchronous review loop: the agent commits while roborev reviews in the background, and the hook interjects to run roborev-fix" loading="lazy">
</figure>

## The Loop

Agents are good at making progress. They are worse at remembering to come back
after a background reviewer finishes, especially when reviews happen out of band
like they do with roborev.

`agent-hook` closes that gap. It sits behind Codex, Claude Code, or Factory
Droid hooks, counts what happened in the current session, checks roborev for
failed reviews, and returns one direct instruction when there is review work to
fix:

```text
Invoke the $roborev-fix skill now.
```

That turns review into part of the agent's normal rhythm: write code, get
reviewed, fix the review, continue.

!!! note

    The default Codex/Claude instruction uses Codex's `$roborev-fix` skill syntax.
    Claude Code refers to the same skill as `/roborev-fix` (see
    [Agent-Specific Syntax](/guides/agent-skills/#agent-specific-syntax)). Factory
    Droid uses a separate default instruction that names `roborev-fix` generically.
    Override `instruction` (see [Configuration](#configuration)) if you prefer
    different wording.

## What It Watches

`agent-hook` tracks three signals per session:

- **Turns.** `Stop` hooks, so long-running sessions get periodic review repair.
- **Commits.** `PostToolUse` shell hooks that produce commits. Codex and Claude
    Code use Bash hooks; Factory Droid uses the `Execute` tool. A matching
    `PreToolUse` hook seeds the per-commit baseline so the count stays accurate.
- **Failed reviews.** Open, non-closed roborev reviews with a failed verdict.

`agent-hook` resolves the repository from the agent's working directory, so
outside a git repository it returns `{}` and stays out of the way. Reminders
also depend on the roborev daemon reporting an open failed review, so a
repository roborev does not track never produces a reminder.

When a Stop hook emits a blocking reminder, the response also tells the agent to
resume the task it was doing after addressing the review findings. This keeps a
review repair from replacing work that paused at a specification, approval, or
implementation checkpoint.

If the main roborev daemon is unavailable, the failed-review check is skipped.
Turn and commit counts still work through the local hook daemon, but they only
prompt the agent once roborev reports at least one open failed review.

Commit-producing shell calls are counted by default, but commit-based prompts
stay off unless `commit_threshold` is set above `0`. Failed-review counts are
scoped to the current git branch. Older jobs without a stored branch are
included, matching `roborev fix` discovery.

## Snoozing Reminders

Silence Agent Hook reminders temporarily when a session needs a longer stretch
of implementation work:

```bash
roborev snooze                 # defaults to eight hours
roborev snooze on --duration 2h
roborev snooze off             # resume immediately
```

The snooze is scoped to the current linked worktree and branch. Switching
branches or working in another checkout does not inherit it. The deadline is
stored as local workspace state in roborev's SQLite database and expires
automatically.

Snoozing affects only the coding-agent harness reminder. Post-commit reviews
continue to enqueue, workers continue processing them, and failed reviews keep
accumulating for later attention. The hook advances its local commit baselines
while snoozed so it does not emit a catch-up reminder for every commit made
during the quiet period.

The bundled `/roborev-snooze` skill (or `$roborev-snooze` in Codex) exposes both
the `on` and `off` operations from an agent session.

## Quick Start

The reminder tells the agent to run the `$roborev-fix` skill, so install
roborev's agent skills first if you have not already:

```bash
roborev skills install
```

Then install the hook entries:

```bash
roborev agent-hook install
```

By default this updates both `~/.codex/hooks.json` and
`~/.claude/settings.json`, registering `PreToolUse`, `PostToolUse`, and `Stop`
hooks. Existing hooks are preserved, and repeated installs are idempotent. Use
`--agent codex` or `--agent claude` to update only one harness, and `--dry-run`
to report what would change without writing.

For Factory Droid, install the Droid profile explicitly:

```bash
roborev agent-hook install --agent droid
```

This updates the user-scoped `~/.factory/hooks.json`, registering `PreToolUse`,
`PostToolUse` (both matching Droid's `Execute` tool), and `Stop` hooks.
Project-scoped Factory hooks are intentionally not supported by roborev because
`.factory/hooks.json` is executable repo-local configuration. Do not commit
Factory hook commands to a repository; install the Droid hook in your user scope
instead.

When roborev is installed through a version manager such as mise,
`agent-hook install` resolves the same stable roborev shim used by
`roborev init`. To pin the exact binary path baked into the agent hook command,
use `--binary`:

```bash
roborev agent-hook install --binary ~/.local/share/mise/shims/roborev
roborev agent-hook install --agent droid --binary ~/.local/share/mise/shims/roborev
```

Use `--command` only when you want to provide the full hook command yourself.
`--binary` and `--command` are mutually exclusive.

For declarative setups (Nix home-manager, dotfiles) where editing those files in
place is the wrong shape, print the JSON for your config system to consume:

```bash
roborev agent-hook dump --agent codex
roborev agent-hook dump --agent claude
roborev agent-hook dump --agent droid --scope user
```

## Runtime Model

Agent harnesses invoke:

```bash
roborev agent-hook run
```

Factory Droid invokes the same runtime with its profile selected:

```bash
roborev agent-hook run --agent droid
```

`run` reads a hook payload on stdin, talks to a small local `roborev-agent-hook`
daemon, and emits the hook response JSON the harness expects. This daemon is
shared by Codex, Claude Code, and Factory Droid profiles. It is separate from
the main roborev daemon and stores only local session counters under:

```text
${ROBOREV_DATA_DIR:-~/.roborev}/agent-hook/
```

The main roborev daemon stays the source of truth for reviews and jobs. `run`
auto-starts the local daemon on demand, and it fails open: if the daemon cannot
be reached or started, it emits `{}` and logs the diagnostic to stderr so a hook
never blocks the agent. Malformed input or a missing `session_id` is treated as
an invalid harness call and returns a normal CLI error.

Manual daemon management is rarely needed, but it works like the main daemon:

```bash
roborev agent-hook daemon start    # no-op if already running
roborev agent-hook daemon status   # running daemons as JSON (PID, version, address, reachability)
roborev agent-hook daemon stop
roborev agent-hook daemon restart  # replace the daemon with the caller's binary
```

## Configuration

Set thresholds in the `[agent_hook]` section of your global config
(`~/.roborev/config.toml`):

```toml
[agent_hook]
turn_threshold = 5
commit_threshold = 0
failed_review_threshold = 4
instruction = "Invoke the $roborev-fix skill now."
```

| Trigger | Default | TOML key | `run` flag | Environment variable |
|---------|---------|----------|------------|----------------------|
| Stop hooks (turns) | `5` | `turn_threshold` | `--turn-threshold` | `ROBOREV_AGENT_HOOK_TURN_THRESHOLD` |
| Commit-producing Bash calls | `0` | `commit_threshold` | `--commit-threshold` | `ROBOREV_AGENT_HOOK_COMMIT_THRESHOLD` |
| Open failed reviews | `4` | `failed_review_threshold` | `--failed-review-threshold` | `ROBOREV_AGENT_HOOK_FAILED_REVIEW_THRESHOLD` |
| Continuation instruction | `Invoke the $roborev-fix skill now.` | `instruction` | `--instruction` | `ROBOREV_AGENT_HOOK_INSTRUCTION` |
| roborev daemon address | runtime discovery | | `--roborev-server` | `ROBOREV_AGENT_HOOK_ROBOREV_ADDR` |

Set any threshold to `0` to disable that trigger. Values resolve in this order,
highest priority first:

```text
run flags > environment variables > [agent_hook] config > defaults
```

`ROBOREV_AGENT_HOOK_ROBOREV_ADDR` and `ROBOREV_AGENT_HOOK_DAEMON_ADDR` are
operational overrides only and are not persisted in TOML.
`ROBOREV_AGENT_HOOK_DAEMON_ADDR` points `run` at a specific local hook daemon
address.

Factory Droid uses its own `[droid_hook]` section and environment variables:

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

Droid values resolve in this order:

```text
run flags > environment variables > [droid_hook] config > defaults
```

`ROBOREV_DROID_HOOK_ROBOREV_ADDR` is an operational override only and is not
persisted in TOML. `ROBOREV_AGENT_HOOK_DAEMON_ADDR` still points `run` at a
specific local hook daemon address shared by every agent-hook profile.

## Inspecting Sessions

Inspect tracked session counters, including `remind_count` (the number of
fix-skill reminders emitted), as JSON. Because the daemon is shared, this shows
sessions from every integration:

```bash
roborev agent-hook status
```

Reset counters when you want a session to start fresh:

```bash
roborev agent-hook reset <session-id>   # reset one session
roborev agent-hook reset --all          # reset every session
```
