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
- Daemon-side inference in `humaEnqueue`, not hook-side, so all local enqueue
  paths and already-deployed hooks are fixed centrally. (User decision.)

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

1. `git for-each-ref --merged=<sha> --sort=-committerdate refs/heads`
   — one command listing local branches whose tips are ancestors of (or equal
   to) the commit.
2. If exactly one tip equals the commit, return that branch. Multiple exact
   tips → ambiguous → `""`.
3. Otherwise, for the top 20 candidates by recent committer date, compute
   distance with `git rev-list --count <tip>..<sha>`; the smallest distance
   wins. A distance tie between two branches → `""`.
4. No ancestor branches → `""`.

Cost: 1 + (capped ancestor count) subprocesses, only on detached-HEAD
enqueues.

### Call site: `humaEnqueue`

After `currentBranch := metadata.CurrentBranch()` returns `""` (and only
when the client sent no branch, and the job type is not insights):

1. Resolve the enqueue target to a commit SHA via `metadata.Resolve`. For
   range refs (`base..HEAD`), resolve the right-hand side. If resolution
   fails, skip inference.
2. `currentBranch = git.InferBranchForCommit(ctx, req.RepoPath, sha)`.
3. When a branch is inferred, log it (`log.Printf`) so the guess is
   observable.

Everything downstream is unchanged: the inferred branch flows into the
existing `IsBranchExcluded` check (exclusion semantics stay consistent) and
into the `req.Branch` fallback that stores the job.

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
  - non-repo path → `""`
- Server-level test in the `server_jobs_test.go` style: enqueue from a
  detached-HEAD worktree and assert the stored job's branch is the inferred
  one; a control case asserts ambiguous setups store `""`.

## Out of scope

- Backfilling existing `branch=""` rows.
- Env-var override (`ROBOREV_BRANCH`) for explicit hints — additive later if
  a guess is ever wrong in practice.
- Hook-side (`roborev post-commit`) changes.
- The unrelated dev-daemon / TUI port confusion observed during the
  investigation.
