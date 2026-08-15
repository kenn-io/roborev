---
title: Agent Hook
description: Bring background roborev findings back into active coding-agent sessions
---

`roborev agent-hook` connects roborev's asynchronous reviews to coding-agent
harness hooks. It records shell-tool and stop events, checks for open failed
reviews, and reminds the active agent to fix them before the session goes cold.

The integration supports every profile in
[`go.kenn.io/kit/agenthook`](https://pkg.go.dev/go.kenn.io/kit/agenthook):

- Claude Code
- Codex
- GitHub Copilot CLI
- Cursor
- Factory Droid
- Gemini CLI
- Hermes Agent
- Qwen Code

Kit owns each harness's native config format, event names, command quoting,
payload normalization, and response encoding. Roborev owns only installed-agent
selection, reminder policy, and local session state.

!!! note

    This differs from [Review Hooks](/guides/hooks/), which run your own shell
    commands when a review completes. Agent Hook plugs into the coding agent's hook
    system to steer the active session itself.

## The Loop

Agent Hook tracks three signals per session:

- **Turns:** `Stop` events, for periodic repair during long sessions.
- **Commits:** normalized shell `PreToolUse` and `PostToolUse` events. Kit maps
    each harness's native shell tool to the common `Bash` vocabulary.
- **Failed reviews:** open, non-closed roborev reviews with a failed verdict.

Roborev scopes commit and failed-review accounting to repository lineage, so
activity in one worktree does not consume another worktree's reminder. Outside a
tracked git repository the hook returns an empty native response.

The default instruction names the exact review job IDs and invokes the
`roborev-fix` skill for only those jobs. It never runs `roborev fix --open` or
discovers additional reviews. The skill treats every finding as an unverified
claim: invalid findings are documented and closed without code changes, valid
in-scope findings are fixed and verified, and valid out-of-scope or unclear
findings remain open until the user gives direction.

Delivered review IDs are acknowledged in the Agent Hook daemon's session state,
scoped to the repository lineage. They do not trigger another reminder in that
session, while newly created review IDs still do. Deferred reminders acknowledge
their IDs only when delivered.

`instruction` is a complete override. Custom instructions are emitted without
the built-in scope or continuation guidance.

## Install

Install hooks for every locally detected coding agent:

```bash
roborev agent-hook install
```

An agent is detected when its executable is on `PATH` or its config directory
already exists. The executable candidates are `claude`, `codex`, `copilot`,
`agent` (Cursor), `droid`, `gemini`, `hermes`, `qwen`, and `grok`.

Select one profile or deliberately install all nine integrations (the eight kit
profiles plus Grok Build):

```bash
roborev agent-hook install --agent qwen
roborev agent-hook install --agent all
```

Use one uniform config override when selecting exactly one agent:

```bash
roborev agent-hook install --agent hermes --config ~/.hermes/config.yaml
```

Automatic and `all` installs attempt every selected profile and report all
errors after preserving successful installs. `--dry-run` plans the same changes
without writing.

For Claude Code, Codex, Factory Droid, and Grok Build, installation also creates
or updates that profile's bundled roborev skills before activating the hook.
Other hook profiles do not currently have bundled skill variants and receive no
CLI fallback.

Factory Droid remains user-scoped. Roborev rejects project `.factory/hooks.json`
paths because they are executable repository-local configuration.

Agent Hook uses the same stable binary resolver as `roborev init`. Pin a shim or
binary explicitly when needed:

```bash
roborev agent-hook install --binary ~/.local/share/mise/shims/roborev
```

`--command` supplies one complete command for an explicit profile. It must
directly invoke `agent-hook run` and select exactly one matching `--agent`;
shell pipelines, chaining, command substitutions, and wrappers are rejected.
Roborev adds its ownership marker before installation. `--command` cannot be
combined with `--binary`.

### Upgrading existing hooks

!!! warning "Stop the old Agent Hook daemon before upgrading"

    If the installed release provides the auxiliary Agent Hook daemon, run that
    release's `roborev agent-hook daemon stop` command before installing or starting
    the new release. The new release removes that command and contains no
    old-process discovery, takeover, or shutdown fallback.

Run `roborev agent-hook install` once after upgrading. The new registrations
carry a feature-specific ownership marker so later installs replace only roborev
agent hooks and preserve unrelated commands. The installer recognizes direct
roborev commands written by the previous Codex, Claude, and Factory Droid
integrations, removes them from that profile's config, and replaces them with
the profile-bearing registration. Unrelated commands and unrecognizable custom
wrappers remain untouched.

The CLI flag migration is:

| Previous invocation | Replacement |
|---------------------|-------------|
| `--codex-config PATH` | `--agent codex --config PATH` |
| `--claude-config PATH` | `--agent claude --config PATH` |
| `--scope user` | Omit the flag; Droid is always user-scoped |
| Installed `agent-hook run` without a profile | Run `roborev agent-hook install` once |

Removed flags are not retained as aliases.

Persisted session state is also read forward during this window. A legacy
session-wide Stop count moves to its single identifiable recent workspace;
ambiguous multi-workspace progress resets instead of being assigned to the wrong
checkout.

## Declarative Config

`dump` requires one profile and writes the complete planned native config to
stdout without modifying the file:

```bash
roborev agent-hook dump --agent codex
roborev agent-hook dump --agent qwen
roborev agent-hook dump --agent hermes
```

JSON-backed harnesses produce JSON. Hermes produces YAML. Use `--config` to
merge an existing file into the plan. Binary-resolution diagnostics stay on
stderr so stdout remains safe to pipe into declarative configuration tooling.

## Runtime Model

Installed commands always identify their profile:

```bash
roborev agent-hook run --agent <profile>
```

`run` requires `--agent`, reads one finite native hook payload from stdin,
passes it through kit's typed dispatcher, posts a normalized request to the
regular roborev daemon, and lets kit encode the native response.

The regular daemon loads and persists session accounting and delivered review
IDs at:

```text
${ROBOREV_DATA_DIR:-~/.roborev}/agent-hook/state.json
```

The same process reads repository registration, review jobs, verdicts, and
workspace snoozes from the review database. Hook communication fails open: a
diagnostic goes to stderr and the harness receives an empty native response.
Invalid native payloads or unsupported profile names remain normal CLI errors.
If the JSON snapshot is unreadable, only Agent Hook event, status, and reset
operations are unavailable; review and queue APIs continue to run, and roborev
does not overwrite the unreadable file. Repair or remove the file, then restart
the regular daemon to load it again.

Persisting reminder state is the at-most-once delivery boundary. Cancellation
observed before that commit leaves a reminder queued; a disconnect after the
commit can consume it because coding-agent hook protocols do not acknowledge
receipt.

### Hermes

Hermes observes post-tool events but cannot inject control output there. When a
post-tool threshold fires, roborev queues a reminder by repository lineage and
trigger type. The next Hermes `Stop` delivers one reminder, ordered by failed
reviews before commits and then creation time.

Queued reminders retain the absolute triggering worktree and tell the agent to
change to it before running review commands, even if the session changed
directories or used `git -C`. Delivery waits until that worktree is back on the
triggering branch, or the exact triggering commit for a detached checkout, so
the fallback commands query the intended lineage. Repeated triggers coalesce
without losing their original queue position. Failed-review reminders are
rechecked before delivery and discarded if the reviews have been resolved.
Commit reminders receive the same recheck, so no queued reminder is delivered
after its failed reviews are resolved.

### Cursor

Cursor sends the same normalized events, thresholds, and accounting requests as
every other profile. Kit v0.14.0 cannot encode control output for Cursor's
post-tool or stop boundaries, so roborev always emits an empty Cursor response.
Only response delivery differs; event handling remains uniform.

## Snoozing Reminders

Silence Agent Hook reminders temporarily when a session needs a longer stretch
of implementation work:

```bash
roborev snooze                 # defaults to eight hours
roborev snooze on --duration 2h
roborev snooze off             # resume immediately
```

The snooze is scoped to the current linked worktree and branch. Switching
branches or working in another checkout does not inherit it. Reviews continue to
enqueue and failed reviews keep accumulating; only the coding-agent reminder is
muted. Hook baselines advance while snoozed, avoiding a catch-up reminder for
every commit made during the quiet period.

Run `roborev status` to list every active snooze with its exact repository,
worktree, branch, and expiry. When the TUI is launched from a snoozed checkout
with automatic repository and branch filters enabled, its title shows the snooze
deadline. Clearing or changing either filter hides the badge because the view no
longer identifies that exact snooze scope.

The bundled `/roborev-snooze` skill (or `$roborev-snooze` in Codex) exposes both
the `on` and `off` operations from an agent session.

## Configuration

Set the top-level global `fix_guidelines` value when agents should validate
review suggestions against a standing policy before editing:

```toml
fix_guidelines = """
Treat review findings as hypotheses. Verify each one against the code and
project requirements. Explain findings that are intentionally not applied.
"""
```

Roborev appends this policy to triggered reminders after the profile's complete
instruction and continuation text. It also reaches direct, batch, and
commit-retry prompts from foreground `roborev fix`. An empty value keeps the
current automatic behavior unchanged.

`fix_guidelines` belongs only in the standard global config. It is separate from
the profile `instruction`, which remains a full replacement for workflow text.
If `agent-hook run --config` selects another file for thresholds or instruction,
fix guidelines still come from the standard global config.

All profiles except Factory Droid use `[agent_hook]` in the global config:

```toml
[agent_hook]
turn_threshold = 5
commit_threshold = 0
failed_review_threshold = 4
instruction = "Resolve open roborev findings now."
```

| Trigger | Default | TOML key | `run` flag | Environment variable |
|---------|---------|----------|------------|----------------------|
| Stop hooks | `5` | `turn_threshold` | `--turn-threshold` | `ROBOREV_AGENT_HOOK_TURN_THRESHOLD` |
| Commits | `0` | `commit_threshold` | `--commit-threshold` | `ROBOREV_AGENT_HOOK_COMMIT_THRESHOLD` |
| Open failed reviews | `4` | `failed_review_threshold` | `--failed-review-threshold` | `ROBOREV_AGENT_HOOK_FAILED_REVIEW_THRESHOLD` |
| Instruction | self-contained fix workflow | `instruction` | `--instruction` | `ROBOREV_AGENT_HOOK_INSTRUCTION` |
| Main daemon address | runtime discovery | | `--roborev-server` | `ROBOREV_AGENT_HOOK_ROBOREV_ADDR` |

Factory Droid keeps `[droid_hook]` and the existing `ROBOREV_DROID_HOOK_*`
environment variables. Its default instruction is now the same self-contained
workflow as every other profile.

Set a threshold to `0` to disable that trigger. Resolution order is:

```text
run flags > environment variables > profile config section > defaults
```

`--roborev-server` and `ROBOREV_AGENT_HOOK_ROBOREV_ADDR` select the regular
roborev daemon used by the hook callback. Address overrides are operational and
are not persisted in TOML.

## Inspecting Sessions

Status includes counters and queued Hermes reminders for every profile:

```bash
roborev agent-hook status
```

Resetting a session also clears its queued reminders:

```bash
roborev agent-hook reset <session-id>
roborev agent-hook reset --all
```

These commands use the regular roborev daemon; there is no separate Agent Hook
process to manage.
