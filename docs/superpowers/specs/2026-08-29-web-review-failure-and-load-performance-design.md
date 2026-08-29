# Web review failure and initial-load performance

## Context

The browser review drawer currently shows `No review output available.` when a
review job fails before it creates a review. The job record already contains
the agent or process failure in its `error` field, but the Review tab does not
render that field. Users must open the log or switch to the terminal UI to learn
why the review failed.

The initial browser review list also waits longer than necessary. After browser
session bootstrap succeeds, the application waits for `/api/status` before it
requests `/api/jobs`. The jobs query then orders the review history with a
normalized timestamp expression that has no matching SQLite index. SQLite must
scan and sort the full job history before returning the first page.

## Goals

- Show the persisted failure reason in the Review tab when a review job fails.
- Reduce the time from successful session bootstrap to the first visible review
  rows.
- Preserve current list ordering, filters, counts, pagination, authentication,
  and reconnect behavior.
- Deliver the failure display and performance work in one pull request.

## Non-goals

- Change how workers classify or persist agent failures.
- Redesign the jobs API or split list rows and counts into separate endpoints.
- Add a cache or denormalized counter table for review statistics.
- Change review log streaming or terminal UI behavior.

## Design

### Failed review state

`ReviewContent` will derive the selected job in addition to the completed review
output. When the selected job has status `failed`, no review output exists, and
the job has a non-empty `error`, the Review tab will show a clear `Review
failed` heading followed by the persisted error text. The text will preserve
line breaks and wrap long lines. Svelte will render it as text so provider or
process output cannot inject markup.

Loading, in-progress, completed-output, and renderer-error states keep their
current precedence. A failed job with no recorded error will use an explicit
failed-without-reason fallback instead of the unrelated no-output message.

### First-page query

SQLite will gain an expression index over the same normalized
`enqueued_at` value and descending job ID used by `ListJobs`. The index will be
created as a new forward migration after any legacy table rebuilds, so existing
databases gain it without modifying shipped migration history and fresh
databases finish with the same schema.

The query and cursor format will not change. SQLite can walk the new index to
return the newest page without building a temporary sort over the full review
history.

### Browser startup

`AppShell` is created only after browser session bootstrap succeeds on the same
daemon that serves the authenticated jobs and status routes. The daemon store
will therefore begin in a provisionally available state. Its existing polling
effect will still request `/api/status` immediately and replace that provisional
state with the observed result.

This lets the review list request and event stream start at the same time as the
first status request instead of waiting behind it. If the status request fails,
the existing unavailable and retry behavior remains authoritative.

## Error handling

- Failure reasons come only from the selected job returned by the authenticated
  browser projection.
- Empty failure reasons produce a specific fallback message.
- A failed initial status request clears provisional availability through the
  current daemon-store failure path.
- A jobs request failure continues to populate the jobs-store error state and
  does not fabricate rows or counts.
- Index creation errors continue to fail database startup rather than silently
  running with an incomplete schema.

## Verification

- A component test will prove that a failed selected job shows its persisted
  reason in the Review tab and does not show the generic no-output state.
- A component test will cover the failed-without-reason fallback.
- An application test will hold `/api/status` pending and prove that
  `/api/jobs` starts without waiting for it.
- Storage migration coverage will open both fresh and prior-schema databases
  and verify that the list-order index is available after migration.
- A query-plan regression test will verify that the first-page ordering uses
  the new index instead of a temporary full-history sort.
- Focused web and storage tests, followed by the repository quality gates, will
  verify that list behavior and existing reconnect behavior remain unchanged.

## Documentation

The web UI documentation will state that failed review jobs show their recorded
failure reason in the Review tab.
