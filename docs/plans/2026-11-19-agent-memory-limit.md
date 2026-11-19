# Proposed per-job agent memory limit

Implemented on Linux. The macOS process table and exit watcher compile and
share their parsing tests with Linux, but have not yet run on native macOS.

## Purpose

An agent can run any command while it reviews or fixes code: a test suite, a
build, a script. One such command can grow until the host runs out of memory.
Today nothing stops it. On a busy daemon, that stalls every other review and
can make the operating system kill the daemon itself.

This proposal adds one setting, `agent_memory_max_mb`. When it is set, the daemon
adds up the resident memory (RSS) of every process that belongs to a running
job. If the sum exceeds the setting, the daemon kills the job's processes and
fails that job with a clear message. Other jobs and the daemon keep running.

The proposal also gives every job ownership of its processes, whether or not the
limit is set. When a job's agent exits, or the job ends, the processes it left
behind are stopped.

What works today:

- The CI checker runs inside a memory-capped `systemd-run --user --scope` on
  Linux hosts that have a user systemd manager (`internal/sandbox`).
- Job timeouts stop a job that runs too long. They do not stop one that uses too
  much memory.

What does not work today:

- Review, fix, synthesis, classify, and other agent jobs have no memory limit.
- Cancelling a job kills only the agent's top process. Commands the agent
  started can keep running after the job ends.
- A grandchild that inherits an agent's stdout can hold a job open until its
  timeout after the agent has exited, because some adapters read stdout to EOF
  before calling `Wait()` (for example, Claude's schema path).

## Goals

- Stop one job from exhausting host memory, on any Linux distribution and on
  macOS, with no system service, init system, or privilege requirement.
- Fail only the job that exceeded the limit, exactly once, with a message that
  names the limit and the largest process.
- Stop every process a job owns when it is no longer needed.
- Never signal a process or process group the job cannot prove it owns.
- Sample nothing when the setting is unset.

## Non-goals

- A hard, kernel-enforced limit. Hosts that need one keep using cgroups, such as
  a systemd unit's `MemoryMax`, as a backstop.
- CPU, disk, or process-count limits.
- Per-agent, per-repository, or per-workflow limits.
- Windows support. The setting is ignored there, with a warning. Process
  ownership on Windows (job objects) is separate work.
- Replacing the CI checker's existing scope limit or its scope cleanup.
- A warning threshold below the limit, or a stored peak-memory value. A job can
  cross both between two samples, and the breach message already gives the
  operator a measurement.

## Anti-goals

These would make the feature a failure even if it technically works:

- Killing an unrelated process or process group because its number was reused.
- Killing a healthy job because of measurement error. A false kill wastes a
  paid agent run and teaches users to turn the setting off.
- Leaving processes running after the daemon reports that it stopped them.
- Making the daemon depend on systemd, cgroup delegation, or root.

## Approaches considered

| Approach | Enforcement | Portability | Decision |
| --- | --- | --- | --- |
| `systemd-run --user --scope` per job | Hard (cgroup) | Needs systemd as init, a running user manager, and a user D-Bus session. Fails on Alpine and other non-systemd distributions, most containers, some WSL setups, and plain SSH sessions without lingering. | Rejected |
| Direct cgroup v2 child groups | Hard (cgroup) | Needs a writable, delegated cgroup subtree. Usually unavailable in containers and under systemd services without `Delegate=yes`. | Rejected as the default; possible later strict mode |
| `RLIMIT_AS` per process | Hard per process | Limits virtual address space, not memory use. Go, Node, V8, and Rust runtimes reserve hundreds of GB of address space they never touch, so normal agents would fail at startup. Also per process, not per job. | Rejected |
| In-daemon watchdog | Polling, best effort | Needs only the process table: `/proc` on Linux, `libproc` on macOS. | **Chosen** |

The watchdog trades hard enforcement for portability. A job can overshoot the
limit between two samples. Hosts that need a hard guarantee add a cgroup limit
around the whole daemon.

## Setting

```toml
# Fail a job when its processes' resident memory (RSS), added together,
# exceeds this many MiB. Shared pages count once per process, so the sum can
# read well above the memory actually used. 0 or unset disables the limit.
# Linux and macOS only.
agent_memory_max_mb = 16384
```

- Global setting in `config.toml`, next to `max_workers`.
- The worker already snapshots global config when an attempt starts. The limit
  is read from that snapshot, so a changed value applies to the next attempt
  without a daemon restart.
- A negative value is a configuration error.
- On Windows, a nonzero value logs one warning at daemon startup and has no
  effect.
- The value is an integer in MiB, like the existing `*_minutes` and `*_seconds`
  settings.

### What the number means

The setting is a **summed-RSS budget**, not a measure of memory use. RSS counts
shared pages, such as shared libraries and copy-on-write pages after `fork`,
once in every process that maps them. Four forked processes that share one
24 MiB allocation report about 123 MiB of summed RSS and about 29 MiB of
proportional set size (PSS). The overcount scales with how much the job's
processes share, not with the size of the limit.

RSS is chosen over PSS because macOS has no PSS equivalent and reading Linux
`smaps_rollup` for every process costs much more than one `stat` read. The docs,
the config comment, and the failure message all say "summed RSS" so operators
size the setting accordingly.

## Ownership model

Every process the daemon starts for a job attempt goes through a **job
tracker**. The worker creates one tracker per attempt and carries it in the
attempt's context.

The tracked launch helper also serves callers without a tracker. Foreground
commands such as `roborev fix` and `roborev refine` call the same adapters
directly, outside the daemon. When the context carries no tracker, and on
Windows, the helper starts and waits for the command exactly as adapters do
today: no process group, no exit watcher, no cleanup.

The tracker owns three kinds of identity:

1. **Roots.** Each process the daemon starts for the attempt, from the list in
   [Which launches count](#which-launches-count). Each root starts in its own
   process group (`Setpgid`), so its PID is also its group ID.
2. **Group members.** Any process whose group ID belongs to a group the tracker
   currently owns (see the next section), found on each sample. A child whose
   parent exited gets a new parent PID but keeps its group, so the group, not the
   parent chain, is how the tracker finds it.
3. **Remembered descendants.** Any process seen, on any sample, as a descendant
   of a root, a group member, or another remembered descendant, by parent PID.
   Live remembered descendants are starting points for later parent walks, so a
   process that left its group with `setsid` keeps contributing the children it
   starts afterward.

A process identity is its PID plus its start time. Each identity is counted
once per sample, however many of these sets it appears in.

### When a group may be signaled

A process group number is safe to signal only while the job can prove the group
still exists and is its own. The rule:

> **A root's process group is owned, and may be signaled, from the root's
> `Start()` until the moment the tracker hands the root to `cmd.Wait()` for
> reaping.** The handoff happens only after the group has been cleaned up, and
> it removes the group from every path that can signal it.

This holds because the kernel does not reuse a PID, and so does not reuse that
PID as a group ID, while the process is running or is an unreaped zombie. Once
the root is reaped and its last group member exits, the number can be reused.
By then the tracker no longer owns it.

### Root lifecycle

All steps that change which groups are owned run under one tracker lock.

1. **Admit.** Before `cmd.Start()`, the launch helper reserves a slot under the
   lock. If cleanup has started, admission fails and nothing is started. A
   refused ACP terminal command never runs.
2. **Start and register.** The helper calls `cmd.Start()` outside the lock,
   then registers the root and its group under the lock. If `Start()` fails, the
   reservation is released. Cleanup waits for every admitted launch to finish
   registering or fail before it signals anything, so an admitted process is
   never missed. A process the root starts before registration is in the root's
   group, so it is covered too.
3. **Watch.** The helper starts one exit watcher per root. The watcher observes
   the root's exit **without reaping it**, using
   `waitid(P_PID, pid, WEXITED|WNOWAIT)` on both Linux and macOS. This also
   returns at once for a root that exited before the watcher started. (A macOS
   kqueue `NOTE_EXIT` filter would not: it is edge-triggered, so a root that
   exits before the filter is registered never produces the event.)
4. **Clean up the group.** When the root exits, the watcher stops the group:
   SIGKILL to the group ID under the lock for an ordinary root, or the root's
   own stop operation for a sandboxed root (below). The stop operation targets
   the sandbox's systemd scope, not a process number, so it runs outside the
   lock. The zombie root still reserves the number.
5. **Hand off.** Still under the lock, the watcher removes the group from the
   owned set, so neither the sampler nor cleanup can signal it again.
6. **Reap.** The launch helper returns a handle. Its `Wait` blocks until the
   handoff, then calls `cmd.Wait()`, which reaps the root and returns its
   original exit status.

The handle's `Wait` is the only code that reaps a tracked root, and it cannot
run before the handoff. Reaping stays with the adapter's own `Wait` call rather
than the watcher because `cmd.Wait()` closes pipes from `StdoutPipe`; reaping
from the watcher would close a pipe the adapter is still reading. Adapters keep
the `*exec.Cmd` for pipes and environment but never call `cmd.Wait()` or
`cmd.Run()` on a tracked command. The behavior tests below fail if a launch site
reaps early.

### Sandboxed roots

The CI checker's sandbox requires that its own cleanup callback stop its
systemd scope. That callback runs `systemctl --user stop`, which sends SIGTERM
so `nono` can remove its files, then escalates to SIGKILL after the scope's
five-second stop timeout.

A sandboxed root registers that callback as its **stop operation**. Wherever
the tracker would SIGKILL an ordinary root's group, it calls the stop operation
instead. The tracker still counts the root's memory and still owns waiting. The
stop operation is not a tracked launch, so it still runs after admission has
closed. A breach in a sandboxed root can therefore run up to five seconds past
the kill decision; the scope's own `MemoryMax` bounds it meanwhile.

### When an individual process may be signaled

Remembered descendants that left their group can be signaled only through an
operation tied to the observed identity:

- **Linux 5.3 and later:** open a pidfd for the PID, read the process's start
  time again, compare it with the remembered one, and signal through
  `pidfd_send_signal`. The pidfd pins the identity, so a PID reused after the
  check cannot receive the signal.
- **macOS and older Linux kernels:** there is no such operation. The tracker
  does not signal the process. It reports cleanup as incomplete and names the
  process.

### Output after the root exits

Killing the root's group releases a grandchild in that group that holds the
agent's stdout. The adapter's read then reaches EOF, and the result the agent
already wrote is preserved.

A pipe holder outside the group is not released that way. Some adapters read
stdout to EOF before waiting (for example, Claude's schema path), so that holder
could keep the job open until its timeout.

- When an adapter gives `os/exec` an `io.Writer`, as `runStreamingCLI` does,
  `os/exec` copies the output itself, and the existing `WaitDelay` already
  bounds that copy once the watcher calls `cmd.Wait()`.
- When an adapter reads a pipe from `StdoutPipe` or `StderrPipe`, as Claude's
  schema path and the ACP agent do, nothing bounds the read today. Once the root
  has exited and its group has been cleaned up, each read on those pipes may
  wait at most one second for more bytes. Buffered output, which includes
  everything the root wrote before it exited, is returned however slowly the
  adapter reads; a read ends the output only after a full second with nothing
  written. `os.Pipe` files on
  Linux and macOS support read deadlines through the runtime poller.

### Which launches count

Roots of the attempt:

| Launch | Notes |
| --- | --- |
| CLI agents through `runStreamingCLI` | Most adapters. |
| Adapters with their own start and wait, such as Droid (`cmd.Run()`) and Claude's schema path | Changed to admit, start, register, and the handle's `Wait`. |
| ACP agent | Uses the tracker from the attempt context. |
| ACP terminal commands | `CreateTerminal` builds its command from `context.Background()`, so the ACP client receives the tracker explicitly when the agent creates it. |
| CI checker package installers and the npm manifest parser | They run package managers and Node on files from the pull request. |
| CI checker `go mod download` | Fetches and unpacks modules the pull request names. |
| CI checker sandbox self-check | A sandboxed root with the scope stop operation. |
| CI checker and rewrite agents | Sandboxed roots with the scope stop operation. |

Not roots, and not counted:

- Git commands the daemon runs to prepare checkouts and worktrees.
- Agent capability probes, such as `codex --help`, which run before an attempt.
- Post-review hooks, which run after the attempt finishes.

Every launch in the first table is reachable only through the tracked helper
when the attempt has a tracker. A launch added later decides which table it
belongs in.

## Sampling

One sampler per daemon, not one per job. It runs only while at least one
running job has a limit.

1. Every 2 seconds (a target cadence, not a guarantee), read the process table
   once.
2. For each running job with a limit, collect its identities as described above
   and sum their RSS.
3. If a job's sum exceeds its limit, record a breach and kill the job (below).

A warm scan of about 460 processes takes about 12 ms on Linux, so one scan per
interval keeps the cost flat as `max_workers` grows.

### Reading the process table

- **Linux:** one read of `/proc/<pid>/stat` per process gives the parent PID,
  group ID, start time, and resident pages. These fields exist on every
  supported kernel, with or without systemd, inside containers, and under WSL 2.
- **macOS:** `proc_listallpids`, then `proc_pidinfo` with `PROC_PIDTASKALLINFO`
  per process, which returns the parent PID, group ID, start time, and resident
  size together. The call must check that the returned size equals the size of
  the structure; a short or zero return is a read failure, never zero RSS. The
  pinned `gopsutil` `MemoryInfo` ignores that return value, so it is not used for
  this. The build stays CGO-free.
- A process that exits during the scan is skipped.
- A known job process that cannot be read, because of permissions, is skipped,
  and the job logs one warning per attempt naming it.
- With Linux `hidepid=2`, processes of other users are absent from `/proc`
  entirely. The daemon cannot count or attribute them. This is a documented
  limit.

## Breach handling

A breach must produce one terminal outcome for the attempt.

### Phases

The tracker has an attempt scope and, while the CI checker runs, a **checker
scope** nested inside it. The checker runs with a child context of the attempt
context, created with the checker scope. Its roots register in the checker
scope. The rewrite phase uses the same checker scope.

When a breach is detected, the tracker attributes it to the innermost open
scope. During the checker phase, the review agents have already exited and
their groups have been cleaned up, so the checker's processes are the job's
remaining processes.

### Order

1. Record the breach under the tracker lock: the scope, the limit, the summed
   RSS, and the largest process (name, PID, RSS). The record is written before
   any signal.
2. Clean up that scope's processes (see [Cleanup](#cleanup)).
3. Cancel that scope's context: the checker's child context for a checker
   breach, the attempt context otherwise.
4. When the code that owns the scope returns, it checks the breach record
   **before** it looks at `ctx.Err()`, retry, failover, or backup logic. If a
   breach is recorded for its scope, the breach decides the outcome, whatever
   error the adapter returned.

User cancellation and interrupt shutdown still win. They cancel the attempt
context, which also cancels the checker's child context. If the attempt context
is canceled when the outcome is decided, the existing cancellation or
interruption handling runs and the breach is only logged.

### Outcomes

| Scope | Outcome |
| --- | --- |
| Attempt (review, fix, synthesis, classify, panel member, and so on) | Fail through the existing attempt-guarded non-retryable path (`failNonRetryableJobLocked`). No retry, no failover, no backup agent. |
| Checker, during the check | Keep the original review's findings and verdict. Store `check.status = "failed"`, write the memory message to the job log, and complete through the existing attempt-guarded completion path. |
| Checker, during the rewrite | Keep the checked review, as a failed rewrite does today, and write the memory message to the job log. |

A checker failure already keeps the review today, including when the checker
hits the sandbox's own 8 GiB scope limit. Failing the whole CI job would discard
a finished, paid review because of the checker's own work.

`checkAndCompleteSynthesis` today returns without saving when the context is
canceled. With a checker scope, a checker breach cancels only the child
context, so the attempt context stays live and completion proceeds.

### Failure message

```text
agent processes' summed RSS reached 17.2 GiB, over agent_memory_max_mb
(16384 MiB); largest: python3 (pid 12345) at 16.9 GiB
```

## Cleanup

Cleanup of a scope stops everything that scope can prove it owns:

1. Close admission for the scope and wait for admitted launches to register or
   fail.
2. For each owned root group: SIGKILL the group, or call the root's stop
   operation for a sandboxed root.
3. For each remembered descendant outside those groups that still exists:
   signal it through a pidfd on Linux 5.3 and later. Otherwise, do not signal
   it, and record it as left running.
4. Wait, up to 15 seconds, for each root's watcher to reach the handoff. A
   process in uninterruptible sleep can delay its exit after SIGKILL, and a
   sandboxed stop takes up to five seconds; a root still running after the wait
   is reported as left running. Reaping stays with the handle's `Wait`, as
   described in [Root lifecycle](#root-lifecycle).
5. If any process could not be signaled, or a signal failed with a permission
   error, log cleanup as incomplete and name the processes.

Cleanup runs:

- for a single root's group, when that root exits (step 4 of the lifecycle);
- for the checker scope, when the checker phase ends for any reason;
- for the attempt scope, when the attempt ends for any reason: completion,
  failure, cancellation, timeout, breach, or shutdown.

SIGKILL rather than SIGTERM for ordinary roots: a process that is filling
memory may not handle SIGTERM in time, and a grace period allows more growth.
The same signal is used for ordinary end-of-job cleanup. Processes left at that
point are leftovers by definition; the agent itself has already exited or been
cancelled.

With the limit unset, nothing is sampled, so a job has no remembered
descendants. Cleanup reaches root groups only. A process that left its group
survives, as every descendant does today.

## Shutdown and Ctrl-C

Today, Ctrl-C on a foreground daemon reaches agents through the terminal's
foreground process group, and the daemon's signal handler calls `Server.Stop`,
which stops new claims and waits for running jobs. Moving agents into their own
groups removes the first effect, so Ctrl-C would wait for whole reviews.

Behavior:

- **SIGINT** (Ctrl-C) uses the existing interrupt shutdown path. It closes
  claims before taking the running-job snapshot, returns running attempts to the
  queue without consuming a retry, lets their trackers clean up, and exits.
- **SIGINT during an update drain.** The interrupt path refuses while an update
  drain is active. In that case the handler logs that the update drain is
  waiting for running jobs and falls back to today's graceful `Server.Stop`, so
  the signal still shuts the daemon down.
- **SIGTERM** keeps today's graceful behavior: stop claims, let running jobs
  finish, then exit. Service managers send SIGTERM and set their own stop
  timeout.
- The API's graceful and interrupt shutdown requests keep their current
  meanings.

## Where the code goes

- `internal/proctrack`: the job tracker and its scopes, the tracked launch
  helper (`Start`, `Run`, `Output`, `CombinedOutput`, `Process.Wait`,
  `Process.Pipe`), process groups, the non-reaping exit watcher, pidfd
  signaling, the shared sampler, and the Linux and macOS process-table readers.
  It is its own package because `internal/agent`, `internal/sandbox`, and
  `internal/daemon` all launch through it, and `internal/agent` already imports
  `internal/sandbox`.
- `internal/agent`: the launch-site changes in the table above.
- `internal/daemon`: creating a tracker for each attempt, the checker scope and
  its child context, breach-first outcome handling in the worker, classifier,
  and checker, and the SIGINT change.
- `internal/sandbox`: the self-check launches through the tracker with the
  check scope's stop as its stop operation.

## Known limits

- **Polling overshoot.** A job can pass the limit by one interval's growth or
  more before the daemon acts. At 1 GiB per second and a 2-second interval, that
  is about 2 GiB; scheduling delay, scan time, and allocation bursts can make it
  larger. Hosts that need a hard limit wrap the daemon in a cgroup limit.
- **Fast detach.** A process that leaves its group with `setsid` before any
  sample sees it is never counted or stopped. Linux could close this gap by
  making the daemon a child subreaper (`PR_SET_CHILD_SUBREAPER`). Not proposed:
  the daemon would inherit orphans from every concurrent job with no way to tell
  which job owns them, and a reaper that calls `wait4(-1)` takes exit statuses
  `os/exec` needs.
- **Detached processes on macOS and old Linux.** Without pidfds, a remembered
  descendant outside its group is not signaled. Its memory still counts, so a
  breach still fails the job, but that process can keep running. The daemon
  reports it.
- **Sandboxed stop delay.** A breach in a sandboxed checker root stops through
  the scope's SIGTERM-then-SIGKILL sequence, up to five seconds. The scope's own
  `MemoryMax` bounds it meanwhile.
- **Detached output.** After a root exits, output that a detached process writes
  to the root's pipes after a one-second pause is discarded. A detached process
  that writes at least once a second holds the adapter's read until the job
  timeout, as it did before this change.
- **Invisible processes.** With `hidepid=2`, other users' processes are absent
  from `/proc`.
- **Processes started outside the job.** A service the agent reaches over a
  socket, such as a Docker daemon running a container, is not part of the job.
  Its memory is not counted.
- **Summed RSS.** See [What the number means](#what-the-number-means).

## Testing

Unit tests with a fake process table:

- group-member discovery after the parent exits, without `setsid`;
- remembered descendants after `setsid`, including children they start later;
- each identity counted once across roots, groups, and remembered descendants;
- PID reuse with a different start time is neither counted nor signaled;
- a process that exits mid-scan;
- a macOS `proc_pidinfo` short return is a read failure, not zero RSS.

Integration tests on Linux and macOS CI, with real processes:

- **Breach:** a root forks a grandchild that allocates past a small limit, with
  a backup agent configured. Assert one failed job, one failure event, zero
  backup invocations, no surviving job processes, and that a concurrent job
  completes. Repeat for a classify job, whose backup runs on any error today.
- **Exit before the watcher:** let a root exit before its watcher starts. Assert
  that cleanup runs and the handle returns the root's original exit status. This
  test is decisive for the macOS watcher and must run on native macOS.
- **Parent exits first:** the tracked root stays alive while an intermediate
  child forks a grandchild that keeps the group, then exits before the first
  sample. Assert the grandchild's RSS is counted and the grandchild is stopped
  at cleanup.
- **Stdout held in the group, limit unset:** the root writes a valid result and
  exits; a sleeping grandchild in its group keeps stdout open. Assert the job
  completes with the result well before its timeout and the grandchild is gone.
- **Stdout held outside the group, limit unset:** the same, with the grandchild
  in its own session. Assert the job completes with the result within the
  one-second output bound.
- **Early terminal exit:** an ACP terminal command exits early in a long job.
  Assert end-of-job cleanup sends no signal to that terminal's former group.
- **Refused terminal:** start cleanup, then request an ACP terminal. Assert the
  command never runs.
- **Cancel:** cancel a job whose agent left a sleeping grandchild. Assert the
  grandchild is gone.
- **Ctrl-C, limit unset:** SIGINT to a foreground daemon with a running review.
  Assert the attempt is requeued without consuming a retry and the daemon exits
  promptly. Repeat during an update drain and assert the graceful fallback.
- **CI checker breach:** read back the stored review. Assert the original
  findings and verdict, `check.status = "failed"`, and the memory message in the
  job log. Repeat for a breach during the rewrite phase and assert the checked
  review is stored.
- **CI checker cleanup:** a breach in the sandboxed checker runs the scope stop
  operation, not a direct group SIGKILL.
- **Foreground callers:** `roborev fix` without a daemon tracker runs its agent
  normally.
- **Windows:** a nonzero setting logs the warning and jobs run normally.

macOS behavior (RSS values, permissions, waiting, and signaling) must be
verified on native macOS CI, not only by cross-compiling.
