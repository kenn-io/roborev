# Post-Commit Enqueue Deduplication

## Problem

Concurrent hook invocations can enqueue multiple identical review jobs for the
same repository and frozen Git reference. Client-side checks cannot prevent
this race because separate processes can all observe an empty queue before any
of them inserts a job.

The daemon must suppress duplicate automatic reviews without preventing a user
from deliberately requesting another review.

## Behavior

Deduplication applies only to enqueue requests whose source is `post_commit`.
After the daemon freezes the requested target, a matching job is identified by
the exact tuple `(repo_id, git_ref, job_type)`.

Any matching job whose status is not `canceled` occupies the deduplication
slot. This includes queued, running, completed, failed, applied, rebased, and
skipped jobs. A canceled job releases the slot and permits a new hook enqueue.

When a duplicate is found, the API returns HTTP 200 with the existing
`EnqueueSkippedResponse` shape. The duplicate request creates no job rows and
does not emit enqueue broadcasts, activity entries, or automatic design-review
follow-ups.

Requests without the `post_commit` source remain deliberate requests and may
always enqueue another review, even when an otherwise matching job exists.

## Storage Design

Storage owns both job-type inference and the atomic duplicate check. The
single-agent and panel enqueue paths each gain a deduplicating variant that:

1. Opens a dedicated SQLite connection.
2. Starts a `BEGIN IMMEDIATE` transaction.
3. Checks for a non-canceled job with the same repository, frozen Git reference,
   and inferred target job type.
4. Returns a duplicate result without inserting when a match exists.
5. Otherwise inserts the single job or complete panel run and commits.

This follows the transaction pattern already used by panel enqueue and prevents
two concurrent daemon handlers from both passing the duplicate check.

For panel runs, the key uses the target/member job type rather than the
synthesis job type. A legitimate panel can therefore retain all of its member
rows and synthesis row while a second hook request for the same target is
suppressed.

Ordinary enqueue methods retain their existing behavior. The daemon selects the
deduplicating storage path only for `post_commit` requests.

## Error Handling

Storage errors remain HTTP 500 enqueue failures. A detected duplicate is an
expected outcome, not an error, and returns HTTP 200 with a reason explaining
that a matching post-commit job already exists.

The transaction rolls back on any lookup, insertion, or commit failure. Panel
enqueue remains all-or-nothing.

## Testing

Tests will cover:

- a second identical `post_commit` request being skipped;
- explicit requests remaining repeatable;
- canceled jobs allowing a replacement enqueue;
- different repositories, frozen references, and job types not colliding;
- concurrent identical hook requests creating exactly one single-agent job;
- duplicate panel requests creating exactly one complete panel run; and
- skipped duplicates producing no enqueue side effects.

The post-commit automation documentation will note that duplicate hook
deliveries for the same target are coalesced by the daemon.
