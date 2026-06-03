# Resilient CI Review — Design

Date: 2026-06-02
Status: Approved (pending spec review)

## Problem

During a codex provider outage, the CI poller posted hard "Review Failed"
comments on PRs and set a failing commit status. Worse, once a panel run is
posted it permanently blocks re-review of that HEAD SHA, so the PR is never
reviewed again unless a new commit supersedes it.

Root cause: `internal/agent/limit.go` `defaultLimitRules` only classifies
**quota** substrings. Provider-outage errors classify as `LimitKindNone` and
are treated as genuine failures. The panel finalize path
(`processSynthesisJob` case 0 in `synthesis_worker.go`, and `panelCommitStatus`
in `ci_poller.go`) then renders "Review Failed" / "All reviews failed". Only
`quota` and `timeout` failures are exempted today.

Observed error strings (captured from `review_jobs` on home-nuc):
- `codex stream reported failure: exceeded retry limit, last status: 429 Too Many Requests, request id: ...`
- `codex stream reported failure: Reconnecting... 2/5 (stream disconnected before completion: An error occurred while processing your request ... help.openai.com ...)`

## Policy (governing contract)

Every PR review attempt for a `(repo, pr, head_sha)` ends in exactly one
terminal state:
1. **Reviewed** — a real combined review comment is posted, OR
2. **Gave up with explanation** — an honest comment is posted after either
   (a) 3 days of repeated transient/outage failures, or
   (b) bounded genuine/deterministic failures.

The old hard "Review Failed / All reviews failed" comment is removed. A
non-success outcome never blocks the PR (status stays non-blocking), and a
later commit always re-triggers a fresh review.

## Failure classes

- **Transient/outage** — `429` / `too many requests`, `exceeded retry limit`,
  `stream disconnected`, `stream reported failure: reconnecting`,
  `connection refused`, `service unavailable` / `502` / `503` /
  `500 internal server error`, `unexpected eof`, `deadline exceeded`,
  `i/o timeout`. Retried indefinitely (with backoff) until success or 3 days.
- **Quota** — existing rules, plus codex `you've hit your usage limit`
  (scoped to codex). Treated as a non-actionable skip (existing behavior).
- **Genuine/deterministic** — everything else that reaches `LimitKindNone`
  (e.g. `model is not supported`, `unknown option`, `stdin is not a terminal`,
  `Model not found`). Retried a bounded number of times (guards
  misclassification), then a soft non-terminal note.

Classification rules must be conservative and tested against the captured
strings: genuine errors above MUST NOT match the transient set.

## Components

### 1. Classification (`internal/agent/limit.go`, `internal/review/result.go`)
- Add transient-producing rules to `defaultLimitRules` (Kind
  `LimitKindTransient`) for the substrings above. Add the codex usage-limit
  quota rule.
- Add `OutageErrorPrefix = "outage: "` in `result.go` (mirrors
  `QuotaErrorPrefix` / `TimeoutErrorPrefix`).
- The worker prepends `OutageErrorPrefix` when a job's *final* failure (retries
  + failover exhausted) classified transient, so the batch layer can recognize
  it. Mirrors the quota-prefix application in `worker.go`.
- `internal/review/synthesis.go`: add `IsTransientFailure(r)` and count it
  alongside `CountQuotaFailures` / `CountTimeoutCancellations`.

### 2. Outcome classifier at finalize (`ci_poller.go` `postPanelRun`)
Before posting, classify the panel run from its member results:
- **≥1 member succeeded** → post combined review (existing path). Members that
  failed transiently render as "skipped (provider unavailable)", not "failed".
- **All failed, ≥1 transient** → *defer* (see retry below); no comment; status
  `pending`.
- **All failed, all genuine** → defer with short backoff while
  `attempt < genuineMaxAttempts`; once exhausted, post the soft non-terminal
  "Review unavailable" note (status `success` + note).
- **Transient 3-day cap reached** → post the "tried for 3 days, provider
  repeatedly unavailable" comment (status `success` + note); stop.

`panelCommitStatus` is updated so transient failures are subtracted from
`realFailures` (like quota/timeout), and the all-failed branch defers instead
of returning `error`.

### 3. Retry state + sweep (`internal/storage`, `ci_poller.go`)
New table `ci_pr_review_attempts` keyed by `(github_repo, pr_number, head_sha)`:

| column            | type    | notes                                            |
|-------------------|---------|--------------------------------------------------|
| github_repo       | text    | part of natural key                              |
| pr_number         | int     | part of natural key                              |
| head_sha          | text    | part of natural key                              |
| attempt           | int     | 1-based count of panel runs enqueued             |
| first_attempt_at  | ts      | when attempt 1 was enqueued (drives 3-day cap)   |
| next_attempt_at   | ts      | when the retry sweep may enqueue the next run    |
| last_error_class  | text    | transient \| genuine \| quota                    |
| state             | text    | pending \| deferred \| reviewed \| gave_up       |
| updated_at        | ts      | sync/debug                                       |

- The attempts row is the **source of truth for "this SHA is being handled"**,
  replacing the panel-active check for dedup. It is created (state `pending`,
  `attempt = 1`, `first_attempt_at = now`) together with the first panel run,
  and updated on every finalize and retry.
- `alreadyReviewedPR` (initial enqueue gate) must skip a SHA when an attempts
  row exists in ANY state (`pending`/`deferred` = in flight; `reviewed`/
  `gave_up` = terminal). This is what prevents the 1m poll from re-enqueuing.
  It can no longer rely on the panel being active, because deferred runs are
  retired (below).
- On a transient/genuine defer: set the attempts row to `deferred` with
  `next_attempt_at`, and retire the just-finished panel run
  (`MarkPanelRetired`) so it cannot double-post. Dedup still holds because the
  attempts row (not the panel) gates enqueue.
- New poller sweep `retryDueReviewAttempts(ghRepo)`: for attempts rows in
  `deferred` with `next_attempt_at <= now`, enqueue a fresh panel run (reuse
  `CreateCIPanelRun` with the same member/synth opts), increment `attempt`, set
  `state = pending`. Runs each poll tick alongside existing sweeps.
- Migration in both `db.go` (SQLite) and `postgres.go` (Postgres), idempotent.
  The table is local retry state for whichever daemon runs the poller; it is
  added to the Postgres schema for parity but is NOT added to the sync cursors
  (retry scheduling is owned by the poller's machine, not synced).

### 4. Backoff schedule
- `delay(n) = min(base * 2^(n-1), 1h)`, `base = 2m`; after the 1h cap, hourly.
- Transient hard cap: `now - first_attempt_at > 72h` → give up (3-day comment).
- Genuine: `genuineMaxAttempts = 3`, short backoff (base 2m), then soft note.
- Constants live in one place (poller or a small `retry` helper) and are unit
  tested.

### 5. Comment templates (`internal/review/synthesis.go`)
- Remove/replace the hard "All review jobs in this batch failed" body.
- New: **transient give-up** — "roborev: Review Unavailable (`sha`) — roborev
  tried to review this PR for 3 days but the AI provider was repeatedly
  unavailable. Last error: <class/excerpt>."
- New: **genuine soft note** — "roborev: Review Unavailable (`sha`) — the review
  agent repeatedly failed to run (likely an agent/config error). It will be
  retried automatically on the next commit. Last error: <excerpt>."
- Both are non-terminal in spirit: a new commit supersedes and re-reviews.

## Edge cases
- **Mixed success**: ≥1 member succeeded → always post the real review now; do
  not defer.
- **PR closed/merged mid-retry**: existing closed-PR cleanup deletes the panel;
  the retry sweep skips rows whose PR is closed and marks the attempt
  `gave_up` (no comment).
- **New HEAD push during retry**: supersede retires the in-flight run and
  clears the attempts row for the old SHA; the new SHA starts fresh at
  attempt 1.
- **Daemon restart**: state is in `ci_pr_review_attempts`; the retry sweep
  resumes from `next_attempt_at`.
- **Throttle interaction**: retries bypass the per-PR throttle (they are
  continuations of one logical review, not new review requests).

## Testing
- Unit: `ClassifyLimit` against every captured error string → correct class;
  genuine strings do not match transient. Backoff/cap math. Outcome classifier
  for all-transient / all-genuine / mixed. New comment renderers.
- Storage: `ci_pr_review_attempts` CRUD + the due-for-retry query, on SQLite
  (and Postgres under the `postgres` tag).
- Integration (daemon): defer → retry → success posts a real review;
  all-genuine → soft note after `genuineMaxAttempts`; simulated >3-day
  transient → give-up comment; partial success renders transient members as
  skipped.

## Out of scope
- Changing per-job retry counts or the failover/cooldown mechanism.
- Backfilling already-posted "Review Failed" comments on existing PRs.
