# Detached-HEAD branch inference at enqueue

**Date:** 2026-07-14
**Status:** Approved

## Problem

When a commit is made in a detached-HEAD checkout (e.g. an SDD/tooling-managed
worktree), the post-commit hook enqueues a review with `branch=""`:

- Hook side: `roborev post-commit` (`cmd/roborev/postcommit.go`) sends
  `gitrepo.CurrentBranch()`, which is empty on detached HEAD.
- Daemon side: `humaEnqueue` (`internal/daemon/server.go`) falls back to
  `metadata.CurrentBranch()` (symbolic-ref / go-git HEAD), also empty.

The job is stored with an empty branch, so every branch-scoped surface —
TUI branch grouping/filtering, `roborev fix`/`refine` open-review discovery,
hook branch-allowlist matching — treats the review as unattributed. Reviews
appear "missing" for the branch the work belongs to.

The common failing shape: a worktree detached at a commit that is one or more
commits **ahead** of its branch ref (the branch has not been synced to the new
commit yet), so "which branch tip points at this commit" finds nothing.

## Decision summary

- Heuristic inference is acceptable; empty branch remains the fallback for
  ambiguous cases. (User decision.)
- Fix future enqueues only; no backfill of existing rows. (User decision.)
- Daemon-side inference in `humaEnqueue`, not hook-side, so already-deployed
  hooks are fixed centrally without a per-repo binary upgrade. (User decision.)
- Covered enqueue shapes: single-commit, range, and dirty targets (everything
  that freezes a `sessionSHA`). Stored-prompt jobs (task/insights/compact)
  have no target commit and are not attributed. (Review finding.)

## Design

### New helper: `internal/git.InferBranchForCommit`

```go
// InferBranchForCommit returns the local branch a detached-HEAD commit most
// likely belongs to, or "" if no unambiguous candidate exists.
func InferBranchForCommit(ctx context.Context, repoPath, sha string) string
```

Subprocess-based, alongside `GetCurrentBranch`. Not added to
`EnqueueMetadataReader`: detached HEAD is the rare path, so it needs no go-git
fast path, and staying off the interface avoids a second implementation.

Algorithm (any git error → return `""`; never fails the enqueue):

1. `git for-each-ref --merged=<sha> refs/heads` — one command listing all
   local branches whose tips are ancestors of (or equal to) the commit.
   Parse full `%(refname)` values and strip `refs/heads/` manually;
   `%(refname:short)` has the ambiguity `GetCurrentBranch` deliberately
   avoids (`internal/git/git.go:346`).
2. If exactly one tip equals the commit, return that branch (unambiguous
   regardless of candidate count). Multiple exact tips → `""`.
3. Otherwise, if more than 20 ancestor candidates exist, fail closed: return
   `""` and log that inference was skipped. Committer-date order does not
   bound distance, so evaluating a truncated subset could miss a closer or
   tied branch; failing closed preserves the unambiguous-candidate contract.
4. Otherwise, compute distance for every candidate with
   `git rev-list --count <tip>..<sha>`; the smallest distance wins. A
   distance tie between two branches → `""`.
5. No ancestor branches → `""`.

All git invocations go through `newGitCmdContext` per the package invariant
(`internal/git/git.go:45`).

Cost: at most 1 + 20 subprocesses, only on detached-HEAD enqueues.

### Call site: `humaEnqueue`, after the target freeze

Inference runs after `buildTargetDescriptor` returns, keyed on the
descriptor's `sessionSHA` — the endpoint SHA the freeze already resolved
(single commit → the commit, range → the end SHA, dirty → HEAD). Resolving
the ref independently before the freeze would race ref movement: the freeze
resolves refs again (`internal/daemon/panel_enqueue.go:229`, `:263`,
`:335`), and a moved HEAD could attribute the branch of one commit to a job
storing another. Reusing `sessionSHA` means the target is resolved exactly
once.

When the client sent no branch, `metadata.CurrentBranch()` was empty
(detached HEAD), the job type is not insights, and `desc.sessionSHA` is
non-empty:

1. `inferred := git.InferBranchForCommit(ctx, checkoutRoot, desc.sessionSHA)`.
2. If `inferred` is empty, proceed unchanged (today's behavior).
3. If `inferred` matches `IsBranchExcluded`, return the same skip response
   the pre-freeze check produces, so exclusion semantics stay consistent
   with symbolic-HEAD enqueues. Because this runs post-freeze, a commit row
   may already have been created before the skip — an idempotent metadata
   row, same as any excluded re-enqueue of an existing commit.
4. Otherwise set the descriptor's branch to `inferred` and log the guess
   (`log.Printf`) so it is observable.

Stored-prompt jobs (task/insights/compact) have an empty `sessionSHA` and
skip inference naturally. Dirty jobs are covered: their `sessionSHA` is the
frozen HEAD of the worktree.

CI jobs are unaffected: the CI poller enqueues directly with `CIBaseBranch`
and never passes through this endpoint's branch fallback, preserving the
invariant that CI jobs leave `Branch` empty.

## Error handling

- Any git subprocess failure, unresolvable ref, non-repo path: inference
  returns `""` and the enqueue proceeds exactly as today.
- Inference never adds a failure mode to the hook path; the only cost is a
  few git subprocesses on the rare detached-HEAD enqueue.

## Testing

- Table-driven unit tests for `InferBranchForCommit` using
  `testutil.NewTestRepo`:
  - exact tip match
  - one-behind ancestor (the SDD worktree shape)
  - nearest of several ancestor branches
  - distance tie → `""`
  - no ancestor branch → `""`
  - two branches pointing at the same commit → `""`
  - more than 20 ancestor candidates and no exact match → `""` (fail
    closed), including a case where the omitted branch would have been
    closer or tied
  - more than 20 candidates with a unique exact tip match → that branch
  - non-repo path → `""`
- Server-level test in the `server_jobs_test.go` style: enqueue from a
  detached-HEAD worktree and assert the stored job's branch is the inferred
  one; a control case asserts ambiguous setups store `""`; a dirty enqueue
  from a detached worktree attributes via the frozen HEAD.

## Out of scope

- Backfilling existing `branch=""` rows.
- Env-var override (`ROBOREV_BRANCH`) for explicit hints — additive later if
  a guess is ever wrong in practice.
- Hook-side (`roborev post-commit`) changes.
- The unrelated dev-daemon / TUI port confusion observed during the
  investigation.
