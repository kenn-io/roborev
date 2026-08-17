# Update Restart Policy Design

## Problem

`roborev update` currently replaces the binary and then synchronously asks the
daemon to shut down. Graceful shutdown stops new job claims and waits without a
deadline for running reviews. This protects review work, but the updater does
not reveal the wait until after installation and gives the user no control over
it. A long review can therefore hold the terminal for an unexpectedly long
time.

The update output also spreads a small amount of information across many
sections, and download progress can run into the following install message.

## Goals

- Tell the user about running reviews before changing the installed binary.
- Let the user wait, abort, or interrupt running reviews and update now.
- Continue accepting new review enqueues during an update drain.
- Prevent queued reviews from starting until the replacement daemon is ready.
- Restart reviews interrupted by the update without consuming a normal retry.
- Report update success only after a responsive daemon reports the installed
  version.
- Keep the non-interactive policy explicit and deterministic.
- Present update details and progress in a compact, stable format.

## Non-goals

- Change the behavior of `roborev daemon stop` or a normal graceful restart.
- Resume an interrupted agent session. An interrupted review starts a fresh
  attempt with its existing job inputs.
- Preserve partial output from an interrupted attempt. The fresh attempt uses
  the existing log truncation behavior, so partial output is discarded.
- Reject enqueues during the drain. Rejecting hook-triggered enqueues could lose
  review requests when the caller does not retry.
- Make an old daemon execute code from the newly installed binary.

## User experience

After finding an update, the command shows one compact summary:

```text
Update available
  Version  v0.64.0 -> v0.65.0
  Package  roborev_0.65.0_linux_amd64.tar.gz (20.3 MB)
  Install  /usr/local/bin/roborev

This replaces a development build with an official release.
```

The source URL and checksum remain available in verbose output. The normal
display names the exact package, size, version, and destination without making
the primary decision harder to scan.

If the daemon has running reviews and no policy flag was supplied, one prompt
both selects the policy and confirms the update before any daemon mutation,
download, or installation:

```text
3 reviews are currently running.

  [w] Wait for them to finish, then update
  [u] Update now; interrupt and restart them automatically
  [a] Abort

Choice [a]:
```

The choices have these semantics:

- `wait`: enter the update drain, let running reviews finish, install, restart,
  and verify.
- `interrupt`: enter the update drain, stop active agent processes cleanly,
  preserve their jobs for a fresh attempt, install, restart, and verify.
- `abort`: exit immediately and leave the daemon and installed binary
  unchanged.

Choosing `wait` or `interrupt` expresses consent to update, so there is no
second `Proceed with update?` prompt. When there are no running reviews, the
existing update confirmation remains sufficient. The updater uses the `wait`
policy if a review starts between the status check and update preparation. This
preserves work without adding a second prompt.

The successful output uses one line per phase and always terminates progress
before printing the next phase:

```text
Downloading  100% (20.3 MB)
Installing   done
Daemon       restarted (v0.65.0)
Git hooks    updated
Skills       updated

Updated roborev to v0.65.0
```

If no daemon was running, the phase block contains `Daemon       not running`
instead of omitting the phase.

Warnings use the same phase label. The command does not print the final
`Updated` line when daemon restart or version verification fails.

## Command-line policy

Add `--running=wait|interrupt|abort` to `roborev update`.

- An explicit value applies whether or not work is active by cutover time.
- Interactive `[a]` exits immediately. The `--running=abort` policy means
  proceed only if the daemon can atomically prepare with no running reviews.
- Interactive `[a]` exits zero, matching today's `Update cancelled` behavior.
  A `--running=abort` busy conflict exits nonzero so automation can distinguish
  a deferred update from an updated or user-canceled command.
- Interactive use without the flag prompts only when the initial status shows
  running reviews.
- `--yes` without `--running` uses `wait`, matching the current safe behavior
  and avoiding a compatibility change for automation.
- A non-interactive wait has no updater-specific deadline. It is bounded by the
  running jobs' configured timeouts, which default to 30 minutes per job.
- `--no-restart` bypasses daemon preparation and the running-review policy. It
  continues to install only the binary and related assets, as it does today.
- The existing `--force` retains its current meaning: allow an official release
  to replace a development build. It does not select interrupt behavior.

Invalid policy values fail before any download, install, or daemon mutation.

## Daemon update preparation

Add daemon endpoints dedicated to preparing, renewing, and releasing an update
drain. Preparation takes the selected running-review policy and an ephemeral
owner ID generated by the updater process. It returns a lease token and expiry.
The updater renews the lease in the background throughout waiting, download,
installation, and pre-shutdown handoff. A 60-second lease renewed every 20
seconds leaves enough tolerance for short scheduling and network stalls without
leaving an abandoned drain for long.

The daemon holds at most one active update lease. Preparing again with the same
owner ID is idempotent, which lets one updater retry a lost response. Preparing
with a different owner while the lease is unexpired returns HTTP 409 with the
remaining lease duration. The updater writes no operation record and takes no
data-directory lock. After a hard process exit, the next updater prepares fresh
once the old lease expires.

Preparation is serialized with shutdown preparation by the daemon lifecycle
mutex. The daemon tracks update-drain ownership and shutdown-drain ownership as
separate in-memory states even though both use the existing persisted
`shutdown_draining` claim gate.

Preparation first persists the existing shutdown drain gate. `ClaimJob`
already checks this gate in the same database statement that claims a queued
job, so the gate creates the required cutover boundary. A claim that wins the
database race becomes running and is included in the active set; later claims
are blocked. Enqueue paths do not check the gate and remain available.

The endpoint then applies the policy:

- `wait`: retain the gate and report the current running count.
- `interrupt`: retain the gate, mark the active jobs as update-interrupted, and
  cancel their worker contexts.
- `abort`: if the running count is nonzero, clear the gate and report a conflict;
  otherwise retain the gate and allow the update.

Preparation does not close the worker pool or request daemon shutdown. Workers
remain available to finish or unwind active work, and the daemon remains able
to accept enqueues and answer status requests while the updater downloads the
release.

Prepare is refused after normal shutdown has begun. If normal stop or SIGTERM
begins while an update lease is active, `beginShutdownDrain` atomically takes
shutdown ownership before invalidating the update lease, so the persisted gate
never opens between owners. Release requires the matching update lease and is
refused after shutdown takes ownership; it can never clear a normal shutdown's
gate.

The daemon automatically releases an update drain when its lease expires. A
wait drain can clear the gate immediately. An interrupt drain first waits for
all targeted worker contexts to unwind and for no targeted job to remain
running, then clears the gate. Until that recovery completes, `/api/status`
continues to expose the drain instead of silently leaving a live daemon unable
to claim work. Re-running `roborev update` reports the active lease until it
expires, then prepares a fresh update. `roborev daemon restart` remains the
explicit recovery that clears stale drain state through normal startup
recovery.

Add `update_draining`, `update_drain_policy`, and
`update_drain_expires_at` fields to `/api/status`. Human-readable
`roborev status` prints the drain beside the worker/queue state, including
whether it is waiting, interrupting, or recovering after lease expiry. Older
clients ignore the additive JSON fields.

## Interrupt and requeue behavior

An update interruption is different from a user cancellation. User-canceled
jobs remain canceled. Update-interrupted jobs must start again automatically.

The worker pool gains an update-interrupt mode with these properties:

1. The daemon records the IDs of jobs that are running at the drain boundary.
2. It cancels each registered worker context so the existing agent process
   cancellation path terminates the child process and captures its output.
3. A job claimed just before the drain but registered just after it observes
   the update-interrupt mode and cancels immediately.
4. One centralized `handleUpdateInterruption` guard requires both a canceled
   context and update-interrupt ownership of the job ID. It runs before the
   normal cancellation, retry, classification, cooldown, and failover logic.
   The shared fail/retry/failover helpers take enough context to invoke this
   guard, so prompt construction, checkout/worktree creation, classification,
   agent execution, patch capture, and synthesis all use the same exit path.
5. The guard atomically changes the matching running attempt back to queued,
   clears the same attempt-scoped session, cost, command, and invocation fields
   as `ResetStaleJobs`, and preserves `retry_count`. Its update is scoped by
   `WHERE status = 'running' AND worker_id = ?`, matching
   `MarkJobAgentInvoked`. If a user cancellation wins the race, the guard
   changes no row and never converts the canceled job back to queued.
6. Update interruption emits no `review.canceled` or terminal event, does not
   release a panel member or synthesis gate, runs no completion hook, and does
   not cool down or fail over an agent. These side effects remain unchanged for
   normal user cancellation and real failures.
7. Graceful shutdown joins the workers, hooks, and final sync as usual. The
   update gate prevents an immediately requeued job from being claimed before
   replacement readiness.
8. Replacement-daemon startup retains the existing `ResetStaleJobs` recovery
   path as a fallback for a process exit that occurs before the centralized
   guard can requeue an interrupted row.

This avoids a process-wide hard kill. Killing only the daemon could orphan an
agent subprocess, while explicitly canceling jobs through the public cancel API
would make them terminal and would not restart them.

## Update sequence

For a running daemon, the updater performs these steps:

1. Check release metadata and display the update summary.
2. Read daemon status and obtain the running-review policy when needed.
3. If reviews are running, use the policy prompt as the install confirmation.
   Otherwise ask for the normal install confirmation unless `--yes` was
   supplied.
4. Ask the daemon to prepare the update with the selected policy.
5. Start the lease-renewal heartbeat. For `wait`, poll until the persisted
   running-job count is zero. For `interrupt`, poll until the worker pool has no
   active workers and no targeted job is still running. A target that completed,
   was canceled by the user, failed, or entered an ordinary retry is already
   unwound and does not block the update. The terminal states why it is waiting
   and updates the remaining count without emitting repeated prose lines.
6. Download, verify, and atomically install the release.
7. Request normal graceful shutdown. Because claims are gated and active work
   is finished, shutdown is bounded by normal completion cleanup rather than
   review duration.
8. Start the installed binary and wait for a responsive daemon.
9. Probe the replacement and require its reported version to equal the release
   version after normalizing one optional leading `v` on both values.
10. Update registered hooks and installed skills with the new binary.
11. Print the final success line.

Hook and skill updates occur only after daemon verification. A failure in those
ancillary updates remains a warning, matching current behavior, but the output
names the affected phase.

When no daemon is running, the updater skips preparation and restart. Successful
binary installation is sufficient for the final success line.

For service-manager restarts, the existing replacement-PID handoff detection
remains in use. Success still requires a responsive daemon with the expected
version.

### Download timing

The update drain remains before download. The current
`selfupdate.Client.Install` API intentionally couples HTTPS download, checksum
verification, temporary-file cleanup, archive verification, and atomic
installation. Separating download in this repository would duplicate
security-sensitive unexported logic; adding a kit staging API is a separate
cross-repository change. The renewable lease and automatic release make the
longer drain window recoverable. A future kit staging API can move download
before preparation without changing the daemon protocol.

### Version skew

The updater must tolerate a daemon from a release that predates the preparation
API. If preparation returns HTTP 404:

- `wait` prints a compatibility notice and falls back to the current synchronous
  graceful-shutdown behavior.
- `interrupt` fails before installation with a message that the running daemon
  does not support safe interruption.
- `abort` proceeds only when `/api/status` reports zero running jobs immediately
  before installation; otherwise it aborts before changing the binary.

The legacy abort fallback cannot provide the new endpoint's atomic claim gate,
so the notice states that a job racing the final status check will be preserved
by the graceful wait. Other preparation errors are not treated as missing API
support. The compatibility fallback changes only drain preparation: replacement
readiness and normalized version verification in steps 8 and 9 remain mandatory
before the updater reports success.

## Failure handling

- Preparation failure: do not download or install.
- Abort conflict: clear any temporary drain and leave the binary unchanged.
- Download or verification failure: release the matching update lease and leave
  the old daemon running. If the updater dies before release, lease expiry
  performs the same recovery.
- Install failure: attempt to release the drain, report the installation error,
  and do not claim success.
- Shutdown failure: do not start a second daemon while the prior daemon or its
  runtime record may still be active.
- Replacement startup failure: report that the binary was installed but daemon
  restart was not confirmed.
- Version mismatch after restart: report both expected and observed versions and
  fail the update command.
- Interrupt cancellation failure: apply a bounded unwind timeout, do not
  install, and leave the leased drain in visible recovery until the workers
  unwind or the user restarts the daemon. Never open the gate while an agent
  process could still duplicate a requeued job.

The updater must handle Ctrl-C while waiting. Before installation it releases
the drain and leaves the current daemon running. After installation, Ctrl-C
stops the updater, releases the update lease if shutdown has not taken
ownership, prints `binary installed; daemon still running old version — run
roborev daemon restart`, and exits nonzero. If shutdown already owns the gate,
release is refused and the message says the daemon is finishing shutdown before
the same recovery command can be run.

## Testing

Focused tests will cover:

- CLI parsing and defaults for all running-review policies.
- The interactive wait, interrupt, and abort decisions.
- `--yes` defaulting to wait and `--no-restart` bypassing preparation.
- Race-safe preparation when a worker claims immediately before the drain.
- Enqueues succeeding while claims remain blocked.
- Abort leaving both drain state and active work unchanged.
- Wait allowing active work to finish without consuming a retry.
- Interrupt terminating the agent process and requeueing the same job without
  consuming a retry.
- Every pre-agent, agent, worktree, classification, patch, and synthesis error
  path using the centralized update-interruption guard.
- Interrupted work emitting no cancel/terminal event, panel release, hook,
  cooldown, or failover side effect.
- A late worker registration being interrupted.
- Lease renewal, expiry, same-owner idempotence, different-owner conflict, and
  automatic gate release without updater-side persistent state.
- Release ownership never clearing a normal shutdown drain.
- Prepare refusing an in-progress shutdown.
- `roborev status` exposing active and recovering update drains.
- Download failure releasing the drain.
- Restart success requiring both daemon responsiveness and the expected version.
- Version verification accepting one optional leading `v`.
- Missing prepare endpoint behavior for wait, interrupt, and abort.
- Interactive abort returning zero and `--running=abort` busy conflict returning
  nonzero.
- Manager-driven restart handoff retaining the same verification requirement.
- Progress output ending cleanly before installation output.
- Stable `Daemon not running` phase output.
- Compact normal output and verbose URL/checksum output.

An isolated integration test will run two synthetic jobs against a scratch data
directory: one active and one enqueued during drain. It will verify that the
active job either completes or is requeued according to policy, the queued job
does not start before replacement readiness, and both run under the replacement
daemon afterward.

## Documentation

Update the command documentation for the new prompt, `--running` flag,
non-interactive defaults, enqueue behavior during drain, and the distinction
between user cancellation and update interruption.
