# Consolidate the Agent Hook Daemon

## Goal

Run Agent Hook session accounting inside the regular roborev daemon and remove
the separate `roborev-agent-hook` daemon without losing persisted session state.

## Architecture

The regular daemon will construct and own the Agent Hook state store alongside
its existing database, worker pool, and API components. On startup, the store
will load the existing JSON snapshot from:

```text
${ROBOREV_DATA_DIR:-~/.roborev}/agent-hook/state.json
```

The regular daemon will expose endpoints for recording a normalized hook event,
listing session state, and resetting one or all sessions. `roborev agent-hook
run`, `status`, and `reset` will discover the regular daemon and use those
endpoints directly.

The Agent Hook package will no longer discover, start, replace, stop, or publish
runtime records for a second daemon. The nested `roborev agent-hook daemon`
command and `ROBOREV_AGENT_HOOK_DAEMON_ADDR` override will be removed.

## State Ownership

The JSON snapshot remains the durable state format. The main daemon loads the
existing file at startup, retains the current in-memory locking and atomic file
replacement behavior, and writes subsequent changes to the same path. Existing
session counters, lineage keys, pending reminders, and delivery boundaries are
therefore materialized unchanged after upgrading.

There is no SQLite migration, dual read/write period, fallback daemon, or new
state format. Once the main daemon has loaded the snapshot, it is the only
process allowed to mutate Agent Hook state.

There is also no legacy-daemon takeover path. The new binary does not discover,
stop, signal, or retain lifecycle support for an already-running Agent Hook
daemon. Operators upgrading from a release with the auxiliary daemon must stop
it before starting the upgraded regular daemon. New hook invocations target the
regular daemon exclusively.

## Data Flow

1. An installed coding-agent hook invokes `roborev agent-hook run --agent ...`.
2. The CLI normalizes the harness payload and discovers the regular daemon.
3. The CLI posts the normalized Agent Hook request to the regular daemon.
4. The daemon evaluates repository scope and open reviews, updates the loaded
   session state, persists the JSON snapshot, and returns the native reminder
   decision.
5. The CLI encodes that decision for the invoking harness.

The handler will use the daemon's existing repository and job data directly
where practical, rather than making an HTTP request back into the same daemon.
An explicit dependency boundary will keep Agent Hook state logic independently
testable and prevent a package import cycle.

## Failure Behavior

Agent Hook callbacks remain fail-open. If the regular daemon is unavailable or
the request fails, the hook writes a diagnostic to stderr and returns an empty
native response. The hook callback will not start an auxiliary daemon.

A brief main-daemon restart may drop hook events during the outage. This is
acceptable because review state is also unavailable during that interval. The
persisted JSON snapshot is reloaded when the regular daemon returns.

If the JSON snapshot cannot be loaded, the regular daemon preserves it without
overwriting it and continues serving reviews and all unrelated features. Agent
Hook event, status, and reset endpoints return a clear state-loading error until
the file is repaired or explicitly removed by the operator. Roborev does not
silently reset corrupt state.

## User-Facing Surface

The following commands remain:

- `roborev agent-hook install`
- `roborev agent-hook dump`
- `roborev agent-hook run`
- `roborev agent-hook status`
- `roborev agent-hook reset`

The `roborev agent-hook daemon` command group is removed. Documentation will
describe Agent Hook as part of the regular daemon and will no longer document
manual lifecycle management for a second process.

## Verification

Focused tests will prove that the regular daemon loads a pre-existing Agent
Hook JSON snapshot, serves hook/status/reset requests against it, persists
updates to the same file, and keeps unrelated daemon APIs available when the
snapshot is invalid. CLI tests will prove direct regular-daemon requests and
fail-open behavior. Existing Agent Hook state-machine tests remain the primary
coverage for reminder and persistence semantics.

Removal of obsolete commands, environment variables, runtime files, and
lifecycle implementation will be verified through the diff, compilation, and
existing tests rather than absence assertions.
