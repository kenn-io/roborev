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

If the daemon has running reviews and no policy flag was supplied, the command
prompts before downloading or installing:

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
- `abort`: if any review is active when the daemon prepares the update, leave
  the daemon and installed binary unchanged.

When there are no running reviews, the existing update confirmation remains
sufficient. The updater uses the `wait` policy if a review starts between the
status check and update preparation. This preserves work without adding a
second prompt.

The successful output uses one line per phase and always terminates progress
before printing the next phase:

```text
Downloading  100% (20.3 MB)
Installing   done
Git hooks    updated
Skills       updated
Daemon       restarted (v0.65.0)

Updated roborev to v0.65.0
```

Warnings use the same phase label. The command does not print the final
`Updated` line when daemon restart or version verification fails.

## Command-line policy

Add `--running=wait|interrupt|abort` to `roborev update`.

- An explicit value applies whether or not work is active by cutover time.
- Interactive use without the flag prompts only when the initial status shows
  running reviews.
- `--yes` without `--running` uses `wait`, matching the current safe behavior
  and avoiding a compatibility change for automation.
- `--no-restart` bypasses daemon preparation and the running-review policy. It
  continues to install only the binary and related assets, as it does today.
- The existing `--force` retains its current meaning: allow an official release
  to replace a development build. It does not select interrupt behavior.

Invalid policy values fail before any download, install, or daemon mutation.

## Daemon update preparation

Add a daemon endpoint dedicated to preparing an update. Its request contains
the selected running-review policy. Preparation is serialized with shutdown
preparation by the daemon lifecycle mutex.

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

Add a matching release operation for failures before shutdown. It clears the
drain gate so workers can claim jobs again. Release is best-effort after an
install failure, but an inability to clear the gate is reported prominently.
Daemon startup continues to clear an interrupted drain as a recovery fallback.

## Interrupt and requeue behavior

An update interruption is different from a user cancellation. User-canceled
jobs remain canceled. Update-interrupted jobs must start again automatically.

The worker pool gains an update-interrupt mode with these properties:

1. The daemon records the IDs of jobs that are running at the drain boundary.
2. It cancels each registered worker context so the existing agent process
   cancellation path terminates the child process and captures its output.
3. A job claimed just before the drain but registered just after it observes
   the update-interrupt mode and cancels immediately.
4. Worker completion detects update interruption and leaves the job in the
   restartable running state instead of failing, canceling, failing over, or
   consuming a retry.
5. Graceful shutdown joins the workers, hooks, and final sync as usual.
6. Replacement-daemon startup uses the existing `ResetStaleJobs` recovery path
   to turn those running rows back into queued rows and clear attempt-scoped
   session and cost metadata.

This avoids a process-wide hard kill. Killing only the daemon could orphan an
agent subprocess, while explicitly canceling jobs through the public cancel API
would make them terminal and would not restart them.

## Update sequence

For a running daemon, the updater performs these steps:

1. Check release metadata and display the update summary.
2. Read daemon status and obtain the running-review policy when needed.
3. Ask for the normal install confirmation unless `--yes` was supplied.
4. Ask the daemon to prepare the update with the selected policy.
5. For `wait`, poll until the persisted running-job count is zero. For
   `interrupt`, poll until the worker pool has no active workers; interrupted
   job rows intentionally remain running until replacement startup requeues
   them. The terminal states why it is waiting and updates the remaining count
   without emitting repeated prose lines.
6. Download, verify, and atomically install the release.
7. Request normal graceful shutdown. Because claims are gated and active work
   is finished, shutdown is bounded by normal completion cleanup rather than
   review duration.
8. Start the installed binary and wait for a responsive daemon.
9. Probe the replacement and require its reported version to equal the release
   version.
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

## Failure handling

- Preparation failure: do not download or install.
- Abort conflict: clear any temporary drain and leave the binary unchanged.
- Download or verification failure: release the update drain and leave the old
  daemon running.
- Install failure: attempt to release the drain, report the installation error,
  and do not claim success.
- Shutdown failure: do not start a second daemon while the prior daemon or its
  runtime record may still be active.
- Replacement startup failure: report that the binary was installed but daemon
  restart was not confirmed.
- Version mismatch after restart: report both expected and observed versions and
  fail the update command.
- Interrupt cancellation failure: apply a bounded unwind timeout, do not
  install, and keep the drain in place while reporting the affected jobs so a
  retry cannot create duplicate work.

The updater must handle Ctrl-C while waiting. Before installation it releases
the drain and leaves the current daemon running. After installation it continues
the safe restart handoff or reports the exact recovery command rather than
silently abandoning a half-complete cutover.

## Testing

Focused tests will cover:

- CLI parsing and defaults for all running-review policies.
- The interactive wait, interrupt, and abort decisions.
- `--yes` defaulting to wait and `--no-restart` bypassing preparation.
- Race-safe preparation when a worker claims immediately before the drain.
- Enqueues succeeding while claims remain blocked.
- Abort leaving both drain state and active work unchanged.
- Wait allowing active work to finish without consuming a retry.
- Interrupt terminating the agent process and requeueing the same job on the
  replacement daemon.
- A late worker registration being interrupted.
- Download failure releasing the drain.
- Restart success requiring both daemon responsiveness and the expected version.
- Manager-driven restart handoff retaining the same verification requirement.
- Progress output ending cleanly before installation output.
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
