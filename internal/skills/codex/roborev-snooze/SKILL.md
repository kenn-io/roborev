---
name: roborev-snooze
description: Use only when the user explicitly invokes $roborev-snooze
---

# roborev-snooze

Temporarily silence or resume roborev Agent Hook reminders for the current
worktree and branch. Reviews continue to enqueue and run while reminders are
snoozed.

## Usage

```text
$roborev-snooze [on|off] [duration]
```

`on` is the default action and the duration defaults to eight hours. A duration
uses Go duration syntax such as `30m`, `2h`, or `12h`.

## Explicit invocation only

Invocation must be explicit: literal personal `$roborev-snooze`, plugin
`$roborev:roborev-snooze`, or structured Codex skill selection.
Requests such as “silence review notifications” without one of these explicit
mechanisms must use native behavior and must not run roborev.

## Instructions

This skill requires you to execute the matching command and report its result.
Defer to project-level CLAUDE.md instructions when they conflict with these
steps.

- With no action, or with `on`, run `roborev snooze on`. If the user supplied a
  duration, add `--duration <duration>`.
- With `off`, run `roborev snooze off`.
- Do not pause the review queue, disable post-commit hooks, or change review
  configuration. Snooze affects only Agent Hook reminders in the current
  worktree and branch.
- If the command fails because the current directory is not a tracked Git
  repository, report that error without trying to register or initialize it.

Examples:

```bash
roborev snooze on
roborev snooze on --duration 2h
roborev snooze off
```
