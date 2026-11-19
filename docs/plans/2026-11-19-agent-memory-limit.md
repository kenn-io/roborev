# Proposed per-job agent memory limit

This is a proposal, not implemented behavior.

## Purpose

An agent can run any command while it reviews or fixes code: a test suite, a
build, a script. One such command can grow until the host runs out of memory.
Today nothing stops it. On a busy daemon, that stalls every other review and
can make the operating system kill the daemon itself.

This proposal adds one setting, `agent_memory_max_mb`. When it is set, the daemon
watches the memory of every process that belongs to a running job. If a job's
processes together exceed the limit, the daemon kills them and fails that job
with a clear message. Other jobs and the daemon keep running.

What works today:

- The CI checker runs inside a memory-capped `systemd-run --user --scope` on
  Linux hosts that have a user systemd manager (`internal/sandbox`).
- Job timeouts stop a job that runs too long. They do not stop one that uses too
  much memory.

What does not work today:

- Review, fix, synthesis, and other agent jobs have no memory limit.
- Cancelling a job kills only the agent's top process. Commands the agent
  started can keep running after the job ends.

## Goals

- Stop one job from exhausting host memory, on any Linux distribution and on
  macOS, with no system service, init system, or privilege requirement.
- Fail only the job that exceeded the limit, with a message that names the limit
  and the largest process.
- Kill every process that belongs to the job when it ends: on completion,
  cancellation, timeout, or exceeding the limit.
- Cost nothing when the setting is unset.

## Non-goals

- A hard, kernel-enforced limit. Hosts that need one keep using cgroups, such as
  a systemd unit's `MemoryMax`, as a backstop.
- CPU, disk, or process-count limits.
- Per-agent, per-repository, or per-workflow limits.
- Windows support. The setting is ignored there, with a warning.
- Replacing the CI checker's existing scope limit.

## Anti-goals

These would make the feature a failure even if it technically works:

- Killing a healthy job because of measurement error. A false kill wastes a
  paid agent run and teaches users to turn the setting off.
- Leaving processes running after the daemon says it killed the job.
- Killing an unrelated process because its PID was reused.
- Making the daemon depend on systemd, cgroup delegation, or root.

## Approaches considered

| Approach | Enforcement | Portability | Decision |
| --- | --- | --- | --- |
| `systemd-run --user --scope` per job | Hard (cgroup) | Needs systemd as init, a running user manager, and a user D-Bus session. Fails on Alpine and other non-systemd distributions, most containers, some WSL setups, and plain SSH sessions without lingering. | Rejected |
| Direct cgroup v2 child groups | Hard (cgroup) | Needs a writable, delegated cgroup subtree. Usually unavailable in containers and under systemd services without `Delegate=yes`. | Rejected as the default; possible later strict mode |
| `RLIMIT_AS` per process | Hard per process | Portable, but limits virtual address space, not memory use. Go, Node, V8, and Rust runtimes reserve hundreds of GB of address space they never touch, so normal agents would fail at startup. Also per process, not per job. | Rejected |
| In-daemon watchdog | Polling, best effort | Needs only the process table: `/proc` on Linux, the kernel process APIs on macOS. | **Chosen** |

The watchdog trades hard enforcement for portability. A process can overshoot
the limit between two samples. Hosts that need a hard guarantee add a cgroup
limit around the whole daemon.

## Setting

```toml
# Kill a job when its processes together use more than this much memory.
# 0 or unset disables the limit. Linux and macOS only.
agent_memory_max_mb = 16384
```

- Global setting in `config.toml`, next to `max_workers`.
- The worker reads the value when a job starts. A changed value applies to new
  jobs without a daemon restart.
- A negative value is a configuration error.
- On Windows, a nonzero value logs one warning at daemon startup and has no
  effect.
- An integer in MiB matches the existing `*_minutes` and `*_seconds` settings and
  needs no size parser. *(Proposer's call; a string such as `"16G"` is the
  alternative.)*

## Which processes belong to a job

A job owns:

1. Every agent process the job starts. This includes CLI agents, ACP agents, and
   the CI checker.
2. Every process the daemon starts on an agent's behalf during the job. Today
   that is ACP terminal commands (`acp_client_terminal.go`). These are children
   of the daemon, not of the agent, so following the agent's process tree alone
   would miss them.
3. Every descendant of those processes, found by following parent PIDs.

The job keeps a set of **roots** (items 1 and 2). Each root is recorded after
`cmd.Start()` succeeds, together with its start time. Descendants are found
again on every sample.

The job also remembers every descendant it has seen, by PID and start time. A
process that detaches itself (`setsid` and a double fork) gets a new parent,
usually PID 1, and drops out of the tree. If the job saw it before it detached,
the job still counts it and still kills it. If the process detached between two
samples, the job never sees it. That gap is accepted (see
[Known limits](#known-limits)).

## Process groups

Every root starts in its own process group (`Setpgid` on Unix). The job can then
signal the root and the descendants that kept its group in one call.

This changes one existing behavior. When a user runs the daemon in the
foreground and presses Ctrl-C, the terminal no longer sends SIGINT to agents.
The daemon already cancels running jobs during shutdown. With this change it
must also wait for the job kill to finish before it exits.

Process groups and kill-on-cancel apply even when the memory limit is unset.
They fix the leftover-process problem above for every user.

## Sampling

One sampler per daemon, not one per job:

1. Every 2 seconds, while at least one job with a limit is running, read the
   process table once.
2. Build a parent-to-children map.
3. For each running job, walk from its roots and remembered descendants and
   sum their resident memory (RSS).
4. If a job's sum exceeds its limit, kill the job (below).

Reading the table once per interval keeps the cost flat as `max_workers` grows.
On a host with a few thousand processes, one pass reads a few thousand small
`/proc` files.

### Reading memory

- **Linux:** read `/proc/<pid>/stat` for the parent PID and start time, and
  `/proc/<pid>/statm` for resident pages. These files exist on every Linux
  kernel the daemon supports, with or without systemd, inside containers, and
  under WSL 2.
- **macOS:** use `proc_listallpids` and `proc_pidinfo` (`PROC_PIDTASKINFO` for
  resident size, `PROC_PIDTBSDINFO` for parent PID and start time). The builder
  chooses between calling them directly and promoting the existing indirect
  `gopsutil` dependency, as long as the build stays CGO-free.
- A process that exits during a read is skipped. A permission error is skipped
  and counted, then logged once per job (see [Open questions](#open-questions)).

### Why RSS

RSS counts pages shared between processes, such as shared libraries and
copy-on-write pages after `fork`, once per process. The sum therefore reads
higher than real use. With a limit in the GiB range, the extra amount is small
next to the limit. Proportional set size (`Pss` from `/proc/<pid>/smaps_rollup`)
is accurate but costs more to read, needs Linux 4.14 or later, and has no macOS
equivalent. *(Proposer's call: RSS.)*

## Killing a job

When a job exceeds its limit:

1. Record the reason and the largest process (name, PID, RSS) before killing
   anything.
2. Send SIGKILL to each root's process group.
3. Send SIGKILL to every remembered descendant that still exists **and** whose
   start time matches the recorded one. The start-time check prevents killing
   an unrelated process that reused the PID.
4. Cancel the job's context so the agent adapter stops reading output and
   returns.
5. Fail the job with:

   ```text
   agent processes used 17.2 GiB, over agent_memory_max_mb (16384 MiB);
   largest: python3 (pid 12345) at 16.9 GiB
   ```

SIGKILL rather than SIGTERM: a process that is filling memory may not handle
SIGTERM in time, and a second grace period allows more growth. *(Proposer's
call.)*

Cancellation, timeouts, and normal job completion use the same kill (steps 2
and 3), so no process from a finished job keeps running. A background process
the agent left behind, such as a test server, is killed when the job ends.
*(Proposer's call.)*

With the limit unset, nothing is sampled, so the job has no remembered
descendants. The end-of-job kill then reaches only the root process groups. A
descendant that left its group survives, as every descendant does today.

### Retries and failover

A memory failure is **not retried and does not fail over** to the backup agent.
A retry repeats the same work on the same checkout. It would likely exceed the
limit again and costs another paid agent run. *(Proposer's call; see
[Open questions](#open-questions).)*

## Where the code goes

- `internal/procutil`: process groups, the platform process-table readers, and
  the group-plus-descendants kill. Unix and Windows files follow the existing
  `command_unix.go` and `command_windows.go` split.
- `internal/agent`: a job-scoped tracker, carried in the job context, that every
  launch registers its root with after `Start()`. `configureSubprocess` runs
  before `Start()`, so registration needs one call after it.
  `runStreamingCLI` covers most adapters. The adapters that call
  `configureSubprocess` directly, the ACP agent launch, and the ACP terminal
  launch each need the call.
- `internal/daemon`: the shared sampler, starting the tracker for each job, and
  the non-retryable failure.

A launch outside a job context, such as a capability probe like `codex --help`,
is not tracked.

## Known limits

- **Polling overshoot.** A job can pass the limit by up to one sampling interval
  of growth before the daemon sees it. At 1 GiB per second and a 2-second
  interval, that is about 2 GiB. Hosts that need a hard limit wrap the daemon in
  a cgroup limit.
- **Fast detach.** A descendant that detaches between two samples is never seen,
  so it is not counted or killed. Linux could close this gap by making the
  daemon a child subreaper (`PR_SET_CHILD_SUBREAPER`). That would make orphaned
  processes children of the daemon, which must then reap them. A reaper that
  calls `wait4(-1)` takes exit statuses that `os/exec` needs for its own `Wait`.
  Not proposed.
- **Processes started outside the job.** A daemon or service the agent reaches
  over a socket, such as a Docker daemon running a container, is not part of the
  job. Its memory is not counted.
- **RSS double counting.** See [Why RSS](#why-rss).

## Testing

- **Unit:** feed the sampler a fake process table. Cover the parent walk,
  remembered detached processes, PID reuse with a different start time, a
  process that exits mid-read, and summing across several roots.
- **Integration (Linux and macOS CI):** start a real child that forks a
  grandchild, which allocates past a small limit. Assert that the job fails with
  the memory message, that no process from the job survives, and that a second
  job running at the same time completes.
- **Cancellation:** cancel a job whose agent left a sleeping grandchild. Assert
  the grandchild is gone.
- **Windows:** a nonzero setting logs the warning and jobs run normally.

## Open questions

1. **Retry policy.** Should a memory failure fail over to the backup agent? A
   different agent might choose different commands.
2. **Default interval.** Is 2 seconds right? A shorter interval narrows
   overshoot and costs more process-table reads per second.
3. **Unreadable processes.** On hardened hosts (`hidepid=2` on `/proc`), a
   descendant running as another user cannot be read. Skip it and log, or fail
   the job? Proposed: skip and log once per job.
4. **Soft warning.** Should the job log a warning at a fraction of the limit,
   such as 80%, before killing at 100%?
5. **Limit display.** Should the job's API record expose the limit and peak
   memory, so the TUI and CI comments can show them?
