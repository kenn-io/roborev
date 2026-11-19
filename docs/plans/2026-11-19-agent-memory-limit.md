# Proposed per-job agent memory limit

This is a proposal, not implemented behavior.

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

Every launch the daemon makes for a job goes through a **job tracker** carried
in the job context. Code that cannot find a tracker starts no tracked process:
the launch helper requires one. Capability probes, such as `codex --help`, run
outside a job and are not tracked.

The tracker owns three kinds of identity:

1. **Roots.** Each process the daemon starts for the job: CLI agents, ACP
   agents, ACP terminal commands, the CI checker's package installers, and the
   CI checker agent. Each root starts in its own process group (`Setpgid`), so
   its PID is also its group ID.
2. **Group members.** Any process whose group ID is an owned root's group, found
   on each sample. A child whose parent exited gets a new parent PID but keeps
   its group, so the group, not the parent chain, is how the tracker finds it.
3. **Remembered descendants.** Any process seen, on any sample, as a descendant
   of a root or group member by parent PID. This catches a process that left the
   group with `setsid` after the tracker saw it.

A process identity is its PID plus its start time. Each identity is counted
once per sample, however many of these sets it appears in.

### When a group may be signaled

A process group number is safe to signal only while the job can prove the group
still exists and is its own. The rule:

> **A root's process group is signaled only while that root has exited but is
> not yet reaped, or is still running.** The root is never reaped until its
> group has been cleaned up.

This holds because the kernel does not reuse a PID, and so does not reuse that
PID as a group ID, while the process is a zombie. Once the root is reaped and
its last group member exits, the number can be reused, and the tracker forgets
the group.

To enforce the rule, the tracker owns waiting:

1. After `cmd.Start()`, the launch helper registers the root and starts an exit
   watcher that observes the root's exit **without reaping it**. Linux uses
   `waitid(P_PID, pid, WEXITED|WNOWAIT)`. macOS uses a kqueue `EVFILT_PROC`
   `NOTE_EXIT` event.
2. When the root exits, the watcher kills the root's group (SIGKILL to the group
   ID). The zombie root still reserves the number.
3. The watcher then allows reaping. The launch helper returns a handle, and the
   handle's `Wait` is the only way to wait: it waits for step 2 and then calls
   `cmd.Wait()`. Adapters keep the `*exec.Cmd` for pipes and environment but do
   not call `cmd.Wait()` or `cmd.Run()` on a tracked command. The behavior tests
   below (stdout held open, early terminal exit) fail if a launch site reaps
   early.

Killing the group when its root exits is what releases a grandchild that holds
the agent's stdout. The adapter's read then reaches EOF and the result the agent
already wrote is preserved.

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

### Registration and cleanup races

- Registration and cleanup share one lock. If cleanup has started, registering a
  new root fails: the launch helper kills that root's group itself (the root is
  unreaped, so the group is safe to signal), reaps it, and returns an error to
  the adapter.
- A process that a root starts between `Start()` and registration is in the
  root's group, so the group kill covers it.

### What each launch site needs

| Launch site | Change |
| --- | --- |
| `runStreamingCLI` (most adapters) | Use the tracked launch helper and the tracker's `Wait`. |
| Adapters that call `configureSubprocess` with `cmd.Run()` or their own `Wait()`, such as Droid and Claude's schema path | Replace with start, register, and the tracker's `Wait`. |
| ACP agent launch | Same, with the job tracker from the job context. |
| ACP terminal commands | `CreateTerminal` builds its command from `context.Background()`, so the ACP client receives the job tracker explicitly when it is created. A terminal started after cleanup begins is refused by the registration rule. |
| CI checker package installers | Run as roots of the job. Their memory counts against the job. |
| CI checker agent | Runs as a root. The sandbox still stops its scope with `systemctl --user stop`; the tracker's group kill is an extra, harmless step. The sandbox's cleanup contract is unchanged. |

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

A breach must produce one terminal outcome for the attempt. The order:

1. Record the breach on the tracker: the limit, the summed RSS, and the largest
   process (name, PID, RSS). The record is written before any signal.
2. Start cleanup (below).
3. Cancel the attempt's context so the adapter returns.
4. When the adapter returns, the worker checks the tracker's breach record
   **before** it looks at `ctx.Err()`, retry, failover, or backup logic. If a
   breach is recorded, the outcome is decided by the breach, whatever error the
   adapter returned.

What the breach decides:

| Job phase | Outcome |
| --- | --- |
| Any agent job (review, fix, synthesis, classify, panel member, and so on) | Fail through the existing attempt-guarded non-retryable path (`failNonRetryableJobLocked`). No retry, no failover, no backup agent. |
| CI checker phase, after the review agent finished | Treat as a checker failure: keep the original review, the same as any other checker failure today, and record the memory message as the checker's error. |

The CI checker row follows the checker's existing contract: a checker failure,
including the sandbox's own 8 GiB scope limit, already keeps the review. Failing
the whole CI job would discard a finished, paid review because of the checker's
own work. *(Proposer's call; the alternative is to fail the job in every phase.)*

The failure message:

```text
agent processes' summed RSS reached 17.2 GiB, over agent_memory_max_mb
(16384 MiB); largest: python3 (pid 12345) at 16.9 GiB
```

Tests run with a backup agent configured and assert: one failed job, one
failure event, and zero backup invocations. The same test covers classify jobs,
whose backup path runs on any error today.

## Cleanup

Cleanup kills everything the job can prove it owns:

1. For each root that is running or unreaped: SIGKILL its process group.
2. For each remembered descendant outside those groups that still exists:
   signal it through a pidfd on Linux 5.3 and later, as described above.
   Otherwise, do not signal it, and record it as left running.
3. Reap the roots through their exit watchers.
4. If any process could not be signaled, or a signal failed with a permission
   error, log cleanup as incomplete and name the processes.

Cleanup runs:

- when a root exits (its own group only, before it is reaped),
- when the job's attempt ends for any reason: completion, failure, cancellation,
  timeout, breach, or shutdown.

SIGKILL rather than SIGTERM: a process that is filling memory may not handle
SIGTERM in time, and a grace period allows more growth. The same signal is used
for ordinary end-of-job cleanup. Processes left at that point are leftovers by
definition; the agent itself has already exited or been cancelled.

With the limit unset, nothing is sampled, so a job has no remembered
descendants. Cleanup reaches roots' groups only. A process that left its group
survives, as every descendant does today.

## Shutdown and Ctrl-C

Today, Ctrl-C on a foreground daemon reaches agents through the terminal's
foreground process group, and the daemon's signal handler calls `Server.Stop`,
which stops new claims and waits for running jobs. Moving agents into their own
groups removes the first effect, so Ctrl-C would wait for whole reviews.

Proposed behavior:

- **SIGINT** (Ctrl-C) uses the existing interrupt shutdown path: running
  attempts return to the queue without consuming a retry, their trackers clean
  up, and the daemon exits.
- **SIGTERM** keeps today's graceful behavior: stop claims, let running jobs
  finish, then exit. Service managers send SIGTERM and set their own stop
  timeout.
- The API's graceful and interrupt shutdown requests keep their current
  meanings.

*(Proposer's call; it changes Ctrl-C from "fail running jobs by killing their
agents" to "requeue running jobs".)*

## Where the code goes

- `internal/procutil`: process groups, the non-reaping exit watcher, pidfd
  signaling, and the Linux and macOS process-table readers. Unix, Linux, macOS,
  and Windows files follow the existing `command_unix.go` and
  `command_windows.go` split.
- `internal/agent`: the job tracker, the tracked launch helper, the tracker's
  `Wait`, and the launch-site changes in the table above.
- `internal/daemon`: creating a tracker for each attempt, the shared sampler,
  breach-first outcome handling in the worker and classifier, the checker-phase
  rule, and the SIGINT change.

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
- **Invisible processes.** With `hidepid=2`, other users' processes are absent
  from `/proc`.
- **Processes started outside the job.** A service the agent reaches over a
  socket, such as a Docker daemon running a container, is not part of the job.
  Its memory is not counted.
- **Summed RSS.** See [What the number means](#what-the-number-means).

## Testing

Unit tests with a fake process table:

- group-member discovery after the parent exits, without `setsid`;
- remembered descendants after `setsid`;
- each identity counted once across roots, groups, and remembered descendants;
- PID reuse with a different start time is neither counted nor signaled;
- a process that exits mid-scan;
- a macOS `proc_pidinfo` short return is a read failure, not zero RSS.

Integration tests on Linux and macOS CI, with real processes:

- **Breach:** a root forks a grandchild that allocates past a small limit.
  Assert one failed job, one failure event, zero backup invocations with a
  backup configured, no surviving job processes, and that a concurrent job
  completes.
- **Parent exits first:** a launcher forks a child that keeps the group, then
  exits before the first sample. Assert the child's RSS is counted and the child
  is stopped at cleanup.
- **Stdout held open, limit unset:** the root writes a valid result and exits;
  a sleeping grandchild keeps stdout open. Assert the job completes with the
  result well before its timeout and the grandchild is gone.
- **Early terminal exit:** an ACP terminal command exits early in a long job.
  Assert end-of-job cleanup sends no signal to that terminal's former group.
- **Cancel:** cancel a job whose agent left a sleeping grandchild. Assert the
  grandchild is gone.
- **Ctrl-C, limit unset:** SIGINT to a foreground daemon with a running review.
  Assert the attempt is requeued without consuming a retry and the daemon exits
  promptly.
- **CI checker breach:** assert the original review is kept and the checker
  records the memory message.
- **Windows:** a nonzero setting logs the warning and jobs run normally.

macOS behavior (RSS values, permissions, and signaling) must be verified on
native macOS CI, not only by cross-compiling.

## Open questions

1. **CI checker phase.** Keep the review on a checker-phase breach (proposed),
   or fail the job in every phase?
2. **Ctrl-C.** Requeue running jobs on SIGINT (proposed), or keep the graceful
   drain and accept that Ctrl-C waits for running reviews?
