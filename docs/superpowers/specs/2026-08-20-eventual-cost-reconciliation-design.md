# Eventual Cost Reconciliation Design

## Problem

Roborev captures token usage immediately after an agent finishes. AgentsView can
index the corresponding session after that lookup window closes. Roborev then
keeps the token counts without a price and does not retry unless an operator runs
`roborev backfill-tokens`.

This timing race makes price coverage depend on how quickly an external indexer
observes a session. A daemon restart does not recover the missed price. The
manual backfill also ignores failed and canceled terminal jobs even when they
have a unique session from an agent invocation.

## Goals

- Eventually record a price when a terminal job has a unique session and the
  configured usage provider later reports a price.
- Keep reconciliation bounded so missing or deleted sessions do not create a
  hot loop.
- Recover after daemon restarts without adding persistent retry bookkeeping.
- Preserve token counts obtained from job logs when a later provider response
  supplies only cost data.
- Make the manual backfill and daemon reconciliation use the same eligibility
  rules.
- Keep completed job processing independent from temporary pricing failures.

## Non-goals

- Estimating prices inside roborev when the configured provider has no price.
- Attributing cumulative usage from a session reused by multiple started jobs.
- Guaranteeing that every terminal row is priceable. Jobs without a session,
  sessions that were deleted, and reused sessions remain unresolved.
- Adding database schema or synchronized retry state.

## Design

### Candidate selection

Storage exposes a paged query for terminal jobs that:

- started and finished;
- have a non-empty session ID;
- do not contain a recorded `cost_usd` value;
- have an agent-run signal (`agent_invoked` or recorded usage); and
- use a session ID associated with exactly one started job.

The query returns only fields needed for reconciliation, orders by job ID, and
accepts an exclusive cursor and a limit. It includes done, applied, rebased,
failed, canceled, and skipped jobs when the remaining conditions hold.

The existing in-memory candidate helpers adopt the same terminal-state rule so
`backfill-tokens` can repair the same jobs.

### Reconciler

The worker pool owns one reconciliation goroutine. It starts and stops with the
pool and has two inputs:

1. A completion signal asks it to revisit a newly finished job soon after the
   initial capture misses a price.
2. A periodic paged scan discovers missed signals, old jobs, and work left by a
   daemon restart.

Each cycle processes a small bounded page. Successful jobs disappear from
future queries because they now contain a recorded price. The cursor advances
through unresolved candidates and wraps after reaching the end, so an
unpriceable session cannot permanently block later jobs.

Provider calls use the configured timeout and run serially in the reconciler.
The interval between scan pages bounds command and provider load. Fresh misses
receive short in-memory retry delays; after those expire, the periodic scan is
the durable backstop. Restarting the daemon discards only timing hints, not the
work itself, because missing price data remains queryable in SQLite.

When usage becomes available, the reconciler merges it with the stored token
data and uses the terminal-job backfill write. The write remains session-scoped
and invalidates the sync cursor so downstream storage receives the updated
price.

Provider-unavailable errors do not fail jobs or stop reconciliation. They are
logged at a bounded rate and retried by a later scan.

### Immediate capture

The existing short lookup remains useful for sessions that become visible
quickly. If it records token data but no price, it signals the reconciler. The
normal job result is not delayed for the longer eventual retry lifecycle.

### Manual recovery

`roborev backfill-tokens` continues to parse per-job logs before querying the
usage provider. Its terminal candidate filters include failed, canceled, and
skipped jobs when they contain evidence that an agent ran. This repairs older
eligible rows and matches the daemon's ongoing behavior.

## Verification

Focused tests cover:

- paged selection, cursor wrap, unique-session filtering, and terminal status
  eligibility;
- a price that appears only after the immediate capture window;
- restart recovery through the persisted candidate scan;
- bounded progress past an unresolved session;
- merging a late price without losing stored token counts; and
- clean reconciler shutdown.

Repository-wide tests, builds, and non-mutating lint checks validate adjacent
daemon and storage behavior.

## Operations

After deploying the verified build, create a database backup, run the token
backfill for the affected reporting window, and replay metrics for the complete
window so downstream dashboards receive corrected rows. Verify coverage from
both roborev analytics and the metrics store.

The dashboard configuration uses UTC as its default timezone so calendar
buckets align with provider reports.
