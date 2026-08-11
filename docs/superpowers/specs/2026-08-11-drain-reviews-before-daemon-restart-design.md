# Drain Reviews Before Daemon Restart

## Goal

No roborev restart may interrupt agent work that is already running. Every
restart path must stop new jobs from starting, tell the user when it is waiting,
wait without a deadline for running jobs to finish, and only then replace the
daemon.

This guarantee applies to:

- `roborev daemon restart`;
- automatic restarts initiated by `ensureDaemon`, including probe and version
  incompatibility restarts; and
- the restart and process-manager handoff performed after `roborev update`.

An explicit `roborev daemon stop` is outside this change. Its existing semantics
remain unchanged.

## Current Failure

The daemon's `Server.Stop` already stops the worker pool gracefully, and the
worker pool waits for active workers. The lifecycle client, however, waits only
two seconds after requesting HTTP shutdown before escalating to an operating
system kill. Reviews routinely exceed two seconds, so restart can terminate an
active agent process before it completes.

Restart logic is also split among the explicit daemon command, automatic
compatibility restarts, and the updater. Fixing only one entry point would leave
the others unsafe.

## Design

### Shared restart drain coordinator

All restart entry points will use one CLI-side coordinator before invoking their
existing stop/start or updater handoff logic. The coordinator will:

1. Read the daemon status and remember the current queue-pause state.
2. Pause queue processing through the existing queue API.
3. Poll daemon status until the number of running jobs is zero.
4. Invoke the restart-specific stop/start operation.
5. Wait for the replacement daemon to become healthy as that restart path does
   today.
6. Restore the original queue-pause state. If the coordinator introduced the
   pause, it unpauses. If the queue was already paused, it remains paused.

The existing persisted queue-pause flag is the drain gate. `ClaimJob` checks
this flag atomically, so queued jobs cannot transition to running after the
pause takes effect. A worker already crossing the pause boundary either remains
queued or becomes visible in the running count and is included in the drain.

The coordinator drains every running daemon job. Review, range, dirty, task,
insights, compact, fix, classify, panel-member, and synthesis work all execute
through the worker pool and must not be terminated by restart.

The coordinator lives in the CLI lifecycle layer. This keeps the policy shared
across restart callers and lets a newly updated CLI safely drain an older daemon
using APIs that already exist in that older daemon. A new daemon-only drain
endpoint would not protect the first update from a version that predates that
endpoint.

### User feedback

When jobs are running, the coordinator writes a message to stderr such as:

```text
Waiting for 2 running reviews to finish before restarting daemon; no new reviews will start...
```

It reports meaningful running-count changes rather than printing on every poll.
When the count reaches zero, it reports that draining is complete and proceeds
with the restart's existing success output.

Messages go to stderr so an automatic restart cannot corrupt JSON or other
structured command output written to stdout. The waiting message is not gated
by verbose mode because the user must know why the command is blocked.

### Indefinite and conservative waiting

The overall drain has no timeout. Individual HTTP requests retain bounded
timeouts so one failed request does not wedge a poll forever.

If status is temporarily unavailable while the daemon process remains alive,
the coordinator must not infer that no work is running. It tells the user that
it is waiting for safe status confirmation and retries indefinitely. It must not
call stop, force-kill, or start a competing daemon while running state is
unknown.

If the daemon exits independently during the drain, the coordinator may proceed
with the restart because it did not terminate the running work. The replacement
daemon's existing recovery behavior handles jobs left in a running state.

If the command context is canceled, the coordinator returns without requesting
shutdown and makes a best effort to restore the original queue-pause state. A
restart failure likewise preserves or restores the pre-restart pause state when
a responsive daemon is available. The restart error remains the primary error;
pause-restoration failures are included as additional context rather than
silently discarded.

## Restart Path Integration

The coordinator accepts the restart-specific replacement operation so policy
and process mechanics remain separate:

- The explicit command uses the normal local daemon stop/start operation.
- `ensureDaemon` uses the same local operation after probe or version checks
  decide a restart is required.
- The updater drains first, then runs its existing PID handoff, process-manager
  detection, old-process cleanup, and updated-binary startup logic.

No restart path may bypass the coordinator. Low-level process-kill helpers stay
available to stop and recovery code, but restart callers may reach them only
after the coordinator has observed zero running jobs.

## Testing

Coordinator tests will use controlled status sequences and call recording to
verify observable ordering and safety:

- queue pause occurs before the first drain poll;
- positive running counts keep waiting and never invoke replacement;
- unavailable status while the daemon is alive keeps waiting and never invokes
  replacement;
- replacement begins exactly after the running count becomes zero;
- count-change and unavailable-status messages are written to stderr;
- a temporary pause is removed after success;
- a pre-existing pause remains set;
- cancellation restores a temporary pause and does not request shutdown;
- replacement errors retain the original pause state and surface restoration
  failures; and
- manual, automatic compatibility, and update restart entry points all invoke
  the shared coordinator.

An integration-level lifecycle test will run a controlled blocking test job,
request restart, enqueue additional work, and verify that the running job
finishes, the queued job does not start during the drain, and the restart only
continues afterward.

The daemon command documentation and changelog will state that every restart
drains running work and prevents new work from starting during the wait.

## Non-goals

- Changing explicit daemon-stop behavior.
- Adding a timeout or force option to restart.
- Adding a new daemon API or database migration.
- Changing normal user-controlled pause and unpause behavior.
