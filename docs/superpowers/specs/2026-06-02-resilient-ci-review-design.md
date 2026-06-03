# Resilient CI Review — Design

Date: 2026-06-02
Status: Pending final approval (revised after spec-review round 2)

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

The old hard "Review Failed / All reviews failed" comment is removed. roborev
**never sets a `failure`/`error` commit status for a transient provider
outage**. While retrying it sets `pending` ("review in progress"), which holds
a PR only when the repo has configured the roborev check as *required* — which
is correct, since the review genuinely has not landed yet. Give-up and
genuine-soft-note outcomes set `success` with an explanatory comment. A later
commit always re-triggers a fresh review.

## Failure classes

- **Transient/outage** — matched conservatively against observed provider
  wording only (same philosophy as the existing quota rules in `limit.go`: no
  speculative substrings that could also match config errors). Default set:
  `too many requests`, `status: 429`, `stream disconnected before completion`,
  `stream reported failure: reconnecting`, and provider HTTP 5xx
  (`500 internal server error`, `502 bad gateway`, `503`, `service
  unavailable`). Bare `connection refused` / `unexpected eof` / `deadline
  exceeded` / `i/o timeout` are **deliberately excluded** — they also describe
  local or config faults and would wrongly hold a PR on the 3-day path; add
  them later only if a captured outage string requires it. Retried with backoff
  until success or 3 days.
- **Quota** — existing rules, plus codex `you've hit your usage limit` (scoped
  to codex). Treated as a non-actionable skip (existing behavior).
- **Genuine/deterministic** — everything else that reaches `LimitKindNone`
  (e.g. `model is not supported`, `unknown option`, `stdin is not a terminal`,
  `Model not found`). Retried a bounded number of times (guards
  misclassification), then a soft non-terminal note.

Classification rules must be conservative and tested against the captured
strings: the genuine examples above MUST NOT match the transient set.

## Components

### 1. Classification (`internal/agent/limit.go`, `internal/review/result.go`)
- Add transient-producing rules to `defaultLimitRules` (Kind
  `LimitKindTransient`) for the observed substrings above. Add the codex
  usage-limit quota rule.
- Add `OutageErrorPrefix = "outage: "` in `result.go` (mirrors
  `QuotaErrorPrefix` / `TimeoutErrorPrefix`).
- The worker prepends `OutageErrorPrefix` when a job's *final* failure (retries
  + failover exhausted) classified transient, so the batch layer can recognize
  it. Mirrors the quota-prefix application in `worker.go`.
- `internal/review/synthesis.go`: add `IsTransientFailure(r)` and count it
  alongside `CountQuotaFailures` / `CountTimeoutCancellations`.

### 2. Outcome classifier at finalize (`ci_poller.go` `postPanelRun`)
Classify the panel run from its member results. **Precedence (first match
wins); quota/timeout members are always counted as skips, never failures:**
1. **≥1 member succeeded** → post combined review (existing path). Failed
   members render as "skipped (provider unavailable)" (transient),
   "skipped (quota)", or "failed" (genuine), as appropriate.
2. **No success, ≥1 transient** → *defer* (see retry); no comment; status
   `pending`. (3-day cap below.)
3. **No success, no transient, ≥1 genuine** → bounded-genuine path: defer with
   short backoff while `consecutive_genuine_attempts < genuineMaxAttempts`; once
   reached, post the soft non-terminal "Review unavailable" note (status
   `success` + note). Quota/timeout members are subtracted as skips.
4. **No success, all skips (quota/timeout only)** → terminal: post the existing
   "Review Skipped" note (status `success` + note); mark `done`. Not retried
   (preserves today's quota behavior).
- **Transient 3-day cap**: in path 2, once `now - first_attempt_at > 72h`, post
  the "tried for 3 days, provider repeatedly unavailable" comment (status
  `success` + note) and mark `done`.

`panelCommitStatus` is updated so transient failures are subtracted from
`realFailures` (like quota/timeout), and the all-failed branch defers (status
`pending`) instead of returning `error`.

### 3. Retry state + sweep (`internal/storage`, `ci_poller.go`)
New table `ci_pr_review_attempts`, `UNIQUE(github_repo, pr_number, head_sha)`:

| column              | type | notes                                              |
|---------------------|------|----------------------------------------------------|
| github_repo         | text | part of natural key                                |
| pr_number           | int  | part of natural key                                |
| head_sha            | text | part of natural key                                |
| attempt             | int  | 1-based count of panel runs enqueued               |
| first_attempt_at    | ts   | when attempt 1 was enqueued (drives 3-day cap)     |
| next_attempt_at     | ts   | when the retry sweep may enqueue the next run      |
| last_error_class    | text | transient \| genuine \| quota                      |
| consecutive_genuine_attempts | int | genuine streak; reset on transient/all-skip/success |
| last_error_excerpt  | text | sanitized last-failure excerpt for give-up comment |
| last_panel_run_uuid | text | most recent run (debug + refetch member errors)    |
| state               | text | pending \| deferred \| done                        |
| updated_at          | ts   | bookkeeping                                        |

- The attempts row is the **source of truth for "this SHA is being handled"**,
  replacing the panel-active check for dedup. All ownership transitions are
  compare-and-set / transactional, mirroring `CreateCIPanelRun`'s reservation
  and `ClaimPanelForPosting`'s CAS:
  - **Reserve (initial enqueue)** — INSERT the attempts row (UNIQUE on
    repo/pr/sha, `state=pending`, `attempt=1`, `first_attempt_at=now`) and
    create the first panel run in **one transaction**. The UNIQUE constraint
    resolves concurrent initial enqueues to a single winner; losers no-op.
  - **`alreadyReviewedPR`** skips a SHA whenever an attempts row exists in any
    state (`pending`/`deferred` in flight, `done` terminal). It no longer relies
    on the panel being active (deferred runs are retired).
  - **Defer** — in **one transaction**: set the row `deferred` with
    `next_attempt_at`, `last_error_class`, `last_error_excerpt`,
    `last_panel_run_uuid`, and retire the finished panel run
    (`MarkPanelRetired`) so it cannot double-post. Dedup still holds (the row,
    not the panel, gates enqueue).
  - **Claim due retry** — `retryDueReviewAttempts(ghRepo)` CAS-claims a due row
    (`UPDATE ... SET state='pending', attempt=attempt+1, next_attempt_at=NULL
    WHERE state='deferred' AND next_attempt_at<=now`; one-row-affected wins),
    then creates the fresh panel run. Only the claim winner enqueues, so two
    sweepers never double-enqueue.
  - **Terminal** — on a real review, skip note, give-up, or soft note, set
    `state='done'`; this blocks re-enqueue at the same SHA until supersede
    (new HEAD) or closed-PR delete.
- **Retry opts are recomputed from current repo/global config** at each enqueue
  (not snapshotted), so a config fix between attempts is picked up
  automatically; `last_panel_run_uuid` links the most recent run for debugging
  and member-error refetch.
- **Closed/merged PR cleanup must cover deferred attempts.** Today's
  `cleanupClosedPRPanels` is panel-driven — it enumerates PRs with *active*
  panels via `GetPendingPanelPRs`. A `deferred` attempt has already retired its
  panel, so it is invisible to that sweep. Cleanup must therefore also enumerate
  PRs that have non-terminal (`pending`/`deferred`) attempts rows, confirm
  closed via `callIsPROpen`, and **delete** those rows (mirroring
  `DeleteCIPanel`). Deleting — not marking `done` — is what lets a reopen at the
  same HEAD SHA start fresh.
- **Retry-sweep close check**: `retryDueReviewAttempts` calls `callIsPROpen`
  before enqueuing a due attempt; if the PR is closed it deletes the attempts
  row instead of enqueuing (stops wasted retries on closed PRs; a later reopen
  starts fresh).
- **Crash recovery** — a reconcile sweep finds `pending` rows whose latest
  panel run is missing or terminal-without-post (e.g. a crash between claim and
  panel create) and re-defers them with a fresh `next_attempt_at`.
- `retryDueReviewAttempts` runs each poll tick alongside the existing sweeps.
- Migration in both `db.go` (SQLite) and `postgres.go` (Postgres), idempotent.
  The table is local retry state for whichever daemon runs the poller; added to
  the Postgres schema for parity but **NOT** to the sync cursors (retry
  scheduling is owned by the poller's machine, not synced).

### 4. Backoff schedule
- `delay(n) = min(base * 2^(n-1), 1h)`, `base = 2m`; after the 1h cap, hourly.
- Transient hard cap: `now - first_attempt_at > 72h` → give up (3-day comment).
- Genuine: `genuineMaxAttempts = 3`, short backoff (base 2m), then soft note.
- `consecutive_genuine_attempts` increments on each genuine defer and resets to
  0 on any non-genuine outcome (transient defer, all-skip terminal, success), so
  a prior transient outage never burns the genuine budget. (`last_error_class`
  records only the latest class, not the streak across mixed outcomes.)
- Constants live in one place (a small `retry` helper) and are unit tested.

### 5. Comment templates (`internal/review/synthesis.go`)
- Remove/replace the hard "All review jobs in this batch failed" body.
- New **transient give-up** — "roborev: Review Unavailable (`sha`) — roborev
  tried to review this PR for 3 days but the AI provider was repeatedly
  unavailable. Last error: <last_error_excerpt>."
- New **genuine soft note** — "roborev: Review Unavailable (`sha`) — the review
  agent repeatedly failed to run (likely an agent/config error). It will be
  retried automatically on the next commit. Last error: <last_error_excerpt>."
- Both are non-terminal in spirit: a new commit supersedes and re-reviews.

## Edge cases
- **Mixed success**: ≥1 member succeeded → always post the real review now; do
  not defer.
- **PR closed/merged mid-retry**: a `deferred` attempt has no active panel, so
  closed-PR cleanup is **attempts-driven, not panel-driven** — it enumerates
  non-terminal attempts rows for the PR, confirms closed, and deletes them (and
  the retry sweep deletes a due attempt whose PR is closed). A reopen at the
  same HEAD SHA then starts fresh — no permanent suppression.
- **New HEAD push during retry**: supersede retires the in-flight run and
  deletes the attempts row for the old SHA; the new SHA starts fresh at
  attempt 1.
- **Daemon restart**: state is in `ci_pr_review_attempts`; the retry sweep
  resumes from `next_attempt_at`; the reconcile sweep repairs interrupted
  claims.
- **Throttle interaction**: retries bypass the per-PR throttle (they are
  continuations of one logical review, not new review requests).

## Testing
- Unit: `ClassifyLimit` against every captured error string → correct class;
  the genuine examples do NOT match transient. Backoff/cap math. Outcome
  classifier across all precedence paths (success / transient / genuine /
  all-skip / mixed). New comment renderers.
- Storage: `ci_pr_review_attempts` reserve/defer/claim/terminal/delete + the
  due-for-retry query, including CAS contention, on SQLite (and Postgres under
  the `postgres` tag).
- Integration (daemon): defer → retry → success posts a real review;
  all-genuine → soft note after `genuineMaxAttempts`; a genuine streak reset by
  an intervening transient does not prematurely give up; simulated >3-day
  transient → give-up comment; partial success renders transient members as
  skipped; closed-while-deferred (no active panel) deletes the attempts row so a
  reopen at the same SHA re-reviews.

## Out of scope
- Changing per-job retry counts or the failover/cooldown mechanism.
- Backfilling already-posted "Review Failed" comments on existing PRs.
