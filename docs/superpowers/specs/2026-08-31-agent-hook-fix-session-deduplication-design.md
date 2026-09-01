# Agent Hook fix-session deduplication

Date: 2026-08-31

Status: Approved

Issue: [#1092](https://github.com/kenn-io/roborev/issues/1092)

## Problem

Agent Hook reminder counters and delivered review IDs are stored per harness
session. When several coding-agent sessions use the same repository lineage,
each session can independently cross a reminder threshold and receive the same
fix instruction. Those sessions can then edit the same working tree at the same
time.

The daemon already serializes Agent Hook state changes under one mutex and
persists each delivered reminder before returning it. It does not have shared
ownership state for a hook-triggered fix.

## Goals

- Deliver at most one hook-triggered fix instruction for a repository lineage
  while its fix session is active.
- Identify the owner by agent profile and harness session ID.
- Release ownership when the hook-triggered workflow reports completion.
- Remind the owner at Stop time when it has not reported completion.
- Expire abandoned ownership after a fixed 12-hour lifetime.
- Persist ownership across daemon restarts.

## Non-goals

- Coordinate direct, human-invoked `roborev fix` or `roborev-fix` work.
- Change review-agent worktree isolation or suppress Agent Hook inside review
  agents.
- Add a configurable lease duration or a separate pending-claim phase.
- Renew ownership when the owner sends ordinary hook activity.

## State model

The Agent Hook snapshot gains a `fix_sessions` map keyed by the existing
repository lineage key. Each entry stores:

- an opaque fix-session ID;
- the agent profile and harness session ID;
- the worktree and branch used for the reminder;
- the start and expiry times;
- an optional completion time.

An entry without a completion time owns its lineage until its fixed expiry.
Ordinary PreToolUse, PostToolUse, and Stop events do not move that expiry.
Completion releases the lineage immediately while retaining the latest
completion state for status output. A later grant for the same lineage replaces
that completed entry.

The event request gains the agent profile. Proper profile-specific hook
registrations always provide it. The temporary profile-less legacy path records
the profile as `legacy` so owner identity remains explicit.

## Reminder delivery

Stop, commit, failed-review, and deferred Hermes reminder paths use the same
locked grant operation immediately before they persist a delivered reminder.
The operation follows these rules:

1. Treat an incomplete entry as inactive when its 12-hour expiry has passed.
2. If another agent/session owns the lineage, emit no fix instruction. Keep the
   candidate session's counters and review IDs eligible for a later event.
3. If no active owner exists, create a fix-session ID and persist the session
   counter changes and ownership in the same snapshot replacement.
4. Return the fix-session ID with the triggered response only after persistence
   succeeds.

If persistence fails, the store restores both the session state and fix-session
state. The hook fails open and no instruction is emitted.

The completion command is appended outside the configured `instruction`. This
keeps custom instructions as full workflow overrides without allowing them to
remove the ownership protocol:

```text
roborev agent-hook fix-done <fix-session-id>
```

The bundled `roborev-fix` skills run the exact command from a direct Agent Hook
instruction after their final audit. They run it when the hook-triggered work
made no edits or left an out-of-scope finding open. Direct skill invocations do
not contain a fix-session ID and do not call the command.

## Owner Stop reminder

Before normal Stop threshold processing, the daemon checks whether the incoming
agent/session owns an active fix session. A normal Stop event from the owner
returns a blocking message that tells the session to finish the interrupted
workflow and run its exact `fix-done` command.

A Stop event with `stop_hook_active=true` remains skipped. This prevents the
closeout reminder from recursively blocking itself. The closeout response does
not append fix guidelines or stale-skill warnings.

## Completion API and CLI

The daemon adds:

```text
POST /api/agent-hook/fix-done
```

The request contains the opaque fix-session ID. The state store finds the
matching current entry, records its completion time, and persists the snapshot.
Calling completion again for the same completed entry succeeds. An unknown or
superseded ID returns an error and cannot release a newer owner.

The CLI exposes the endpoint as:

```text
roborev agent-hook fix-done <fix-session-id>
```

`roborev agent-hook status` adds fix-session state to its existing JSON output.
Resetting one harness session clears an active fix session owned by that exact
session. `roborev agent-hook reset --all` clears all fix-session state.

## Testing

State-store and daemon tests cover observable behavior:

- simultaneous Stop triggers on one lineage grant exactly one fix session;
- simultaneous commit triggers on one lineage grant exactly one fix session;
- simultaneous failed-review triggers on one lineage grant exactly one fix
  session;
- different lineages can each grant one fix session;
- owner Stop returns the closeout reminder and recursive Stop is skipped;
- completion releases immediately and repeated completion is idempotent;
- an old fix-session ID cannot release a newer owner;
- ordinary hook activity does not move the 12-hour expiry;
- an expired owner permits a new grant;
- ownership survives loading the persisted snapshot;
- a failed snapshot write grants no ownership and emits no instruction;
- normal, Grok, deferred Hermes, and legacy outputs include the matching
  completion command.

The focused package tests run before the repository-wide Go tests and race
tests for the Agent Hook packages.

## Documentation

The Agent Hook guide will explain the single-owner behavior, the fixed 12-hour
expiry, the owner Stop reminder, the completion command, status output, and
reset behavior. It will state that direct fixes are not coordinated by this
mechanism.
