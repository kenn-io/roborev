# Detached-HEAD Branch Inference Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Commits made on detached HEAD (e.g. tooling-managed worktrees) get their review jobs attributed to the branch they belong to, instead of `branch=""`.

**Architecture:** A new subprocess helper `git.InferBranchForCommit` finds the unique branch whose tip equals or is the nearest ancestor of a commit. `humaEnqueue` calls it after the target freeze, keyed on the descriptor's already-resolved `sessionSHA`, and threads the inferred branch through exclusion checks, the descriptor, and session reuse.

**Tech Stack:** Go, git subprocesses via the `internal/git` package runner, testify, httptest.

**Spec:** `docs/superpowers/specs/2026-07-14-detached-head-branch-inference-design.md`

## Global Constraints

- All git invocations in `internal/git` go through `newGitCmdContext` (package invariant, `internal/git/git.go:45`).
- Parse full `%(refname)` values and strip `refs/heads/` manually — never `%(refname:short)` (ambiguity, see `GetCurrentBranch` doc comment at `internal/git/git.go:346`).
- Inference never fails an enqueue: any git error returns `""`.
- Fail closed above 20 non-exact ancestor candidates; a unique exact tip match wins at any candidate count.
- After Go changes: `go fmt ./...` and `go vet ./...`; stage ALL resulting changes.
- Tests use testify (`assert`/`require`); table-driven or subtests; `t.TempDir()` isolation.
- Never amend commits; every fix is a new commit.

---

### Task 1: `git.InferBranchForCommit` helper

**Files:**
- Modify: `internal/git/git.go` (add after `GetCurrentBranch`, ~line 362)
- Test: `internal/git/git_test.go` (add after `TestGetCurrentBranch`, ~line 1128)

**Interfaces:**
- Consumes: `newGitCmdContext(ctx, args...)` (existing package helper), test helpers `NewTestRepoWithCommit(t)`, `repo.Run(...)`, `repo.CheckoutNewBranch(name)`, `repo.CommitFile(name, content, msg)` (defined in `internal/git/git_test.go`, package-local).
- Produces: `func InferBranchForCommit(ctx context.Context, repoPath, sha string) string` — `sha` must be a full commit SHA; returns `""` when no unambiguous candidate exists. Task 2 calls this from `internal/daemon`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/git/git_test.go` after `TestGetCurrentBranch`. The package-local `TestRepo.CommitFile` returns nothing, so capture SHAs with `repo.HeadSHA()`. Note `NewTestRepoWithCommit` creates one commit on the default branch (call it via `GetCurrentBranch(repo.Dir)` when needed).

```go
func TestInferBranchForCommit(t *testing.T) {
	ctx := context.Background()

	t.Run("exact tip match", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.CheckoutNewBranch("feature")
		repo.CommitFile("f.txt", "content", "feature commit")
		sha := repo.HeadSHA()
		repo.Run("checkout", "--detach")

		assert.Equal(t, "feature", InferBranchForCommit(ctx, repo.Dir, sha))
	})

	t.Run("one-behind ancestor (detached worktree shape)", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.CheckoutNewBranch("feature")
		repo.CommitFile("f.txt", "content", "feature commit")
		repo.Run("checkout", "--detach")
		repo.CommitFile("g.txt", "content", "detached commit")
		sha := repo.HeadSHA()

		// feature tip is 1 behind sha; the default branch is 2 behind.
		assert.Equal(t, "feature", InferBranchForCommit(ctx, repo.Dir, sha))
	})

	t.Run("nearest of several ancestor branches", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.CheckoutNewBranch("far")
		repo.CommitFile("a.txt", "a", "far commit")
		repo.CheckoutNewBranch("near")
		repo.CommitFile("b.txt", "b", "near commit")
		repo.Run("checkout", "--detach")
		repo.CommitFile("c.txt", "c", "detached commit")
		sha := repo.HeadSHA()

		assert.Equal(t, "near", InferBranchForCommit(ctx, repo.Dir, sha))
	})

	t.Run("distance tie returns empty", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.CheckoutNewBranch("feature")
		repo.CommitFile("f.txt", "content", "shared tip")
		repo.Run("branch", "twin") // second branch at the same tip
		repo.Run("checkout", "--detach")
		repo.CommitFile("g.txt", "content", "detached commit")
		sha := repo.HeadSHA()

		assert.Empty(t, InferBranchForCommit(ctx, repo.Dir, sha))
	})

	t.Run("two exact tips returns empty", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.CheckoutNewBranch("feature")
		repo.CommitFile("f.txt", "content", "shared tip")
		repo.Run("branch", "twin")
		sha := repo.HeadSHA()
		repo.Run("checkout", "--detach")

		assert.Empty(t, InferBranchForCommit(ctx, repo.Dir, sha))
	})

	t.Run("no ancestor branch returns empty", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		branch := GetCurrentBranch(repo.Dir)
		repo.Run("checkout", "--detach")
		repo.CommitFile("f.txt", "content", "detached commit")
		sha := repo.HeadSHA()
		repo.Run("branch", "-D", branch)

		assert.Empty(t, InferBranchForCommit(ctx, repo.Dir, sha))
	})

	t.Run("more than 20 candidates fails closed", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		// 21 ancestor branches at increasing depth; nearest would be b21.
		for i := 1; i <= 21; i++ {
			repo.CommitFile("f.txt", fmt.Sprintf("v%d", i), fmt.Sprintf("c%d", i))
			repo.Run("branch", fmt.Sprintf("b%d", i))
		}
		repo.Run("checkout", "--detach")
		repo.CommitFile("g.txt", "content", "detached commit")
		sha := repo.HeadSHA()

		// b21 is distance 1 and would win, but 21 non-exact candidates
		// (plus the default branch, 22 total) exceed the cap: fail closed
		// rather than rank a truncated subset.
		assert.Empty(t, InferBranchForCommit(ctx, repo.Dir, sha))
	})

	t.Run("unique exact match wins above the cap", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		for i := 1; i <= 21; i++ {
			repo.CommitFile("f.txt", fmt.Sprintf("v%d", i), fmt.Sprintf("c%d", i))
			repo.Run("branch", fmt.Sprintf("b%d", i))
		}
		repo.Run("checkout", "--detach")
		repo.CommitFile("g.txt", "content", "detached commit")
		sha := repo.HeadSHA()
		repo.Run("branch", "exact-tip")

		assert.Equal(t, "exact-tip", InferBranchForCommit(ctx, repo.Dir, sha))
	})

	t.Run("non-repo returns empty", func(t *testing.T) {
		assert.Empty(t, InferBranchForCommit(ctx, t.TempDir(), "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"))
	})
}
```

If `fmt` or `context` are not already imported in `git_test.go`, add them.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/git/ -run TestInferBranchForCommit -v`
Expected: FAIL — `undefined: InferBranchForCommit`

- [ ] **Step 3: Implement the helper**

Add to `internal/git/git.go` after `GetCurrentBranch` (after line 362). Add `"log"` and `"strconv"` to the import block if absent.

```go
// inferBranchMaxCandidates bounds how many non-exact ancestor branches
// InferBranchForCommit will rank by distance. Above this it fails closed:
// committer-date or listing order does not bound distance, so ranking a
// truncated subset could miss a closer or tied branch.
const inferBranchMaxCandidates = 20

// InferBranchForCommit returns the local branch a detached-HEAD commit most
// likely belongs to: the unique branch whose tip equals the commit, or
// failing that the unique branch whose tip is the nearest ancestor of it.
// sha must be a full commit SHA. It returns "" when no unambiguous
// candidate exists (ties, more than inferBranchMaxCandidates ancestor
// branches, git errors, non-repos), in which case the job keeps an empty
// branch exactly as before inference existed.
func InferBranchForCommit(ctx context.Context, repoPath, sha string) string {
	cmd := newGitCmdContext(ctx, "for-each-ref", "--merged="+sha,
		"--format=%(objectname) %(refname)", "refs/heads")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	var exact, candidates []string
	tips := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		tip, ref, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		branch := strings.TrimPrefix(ref, "refs/heads/")
		if tip == sha {
			exact = append(exact, branch)
			continue
		}
		candidates = append(candidates, branch)
		tips[branch] = tip
	}

	if len(exact) == 1 {
		return exact[0]
	}
	if len(exact) > 1 || len(candidates) == 0 {
		return ""
	}
	if len(candidates) > inferBranchMaxCandidates {
		log.Printf(
			"infer branch: %d ancestor branches of %s exceed cap %d, skipping inference",
			len(candidates), sha, inferBranchMaxCandidates,
		)
		return ""
	}

	best := ""
	bestDist := -1
	for _, branch := range candidates {
		dist, ok := commitDistance(ctx, repoPath, tips[branch], sha)
		if !ok {
			return ""
		}
		switch {
		case bestDist == -1 || dist < bestDist:
			best, bestDist = branch, dist
		case dist == bestDist:
			best = "" // tie: ambiguous unless a closer branch follows
		}
	}
	return best
}

// commitDistance returns the number of commits reachable from to but not
// from from (git rev-list --count from..to).
func commitDistance(ctx context.Context, repoPath, from, to string) (int, bool) {
	cmd := newGitCmdContext(ctx, "rev-list", "--count", from+".."+to)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, false
	}
	return n, true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/git/ -run TestInferBranchForCommit -v`
Expected: PASS (all 9 subtests)

Also run the package to catch regressions: `go test ./internal/git/`
Expected: ok

- [ ] **Step 5: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add -A
git commit -m "Add git.InferBranchForCommit for detached-HEAD attribution"
```

---

### Task 2: Wire inference into humaEnqueue

**Files:**
- Modify: `internal/daemon/server.go` (humaEnqueue, immediately after the `buildTargetDescriptor` early-return at ~line 2061-2063)
- Test: `internal/daemon/server_jobs_test.go`

**Interfaces:**
- Consumes: `git.InferBranchForCommit(ctx, repoPath, sha string) string` from Task 1 (the `internal/git` package is already imported in server.go as `git`); existing `config.IsBranchExcluded(repoPath, branch string) bool`; `descriptor.sessionSHA` / `descriptor.branch` fields of `targetDescriptor` (internal/daemon/panel_enqueue.go:27).
- Produces: enqueue behavior only; no new exported symbols.

- [ ] **Step 1: Write the failing tests**

Add to `internal/daemon/server_jobs_test.go` (imports `testutil`, `storage`, `gitpkg "go.kenn.io/roborev/internal/git"`, `httptest`, `testify` are already present). `testutil.InitTestGitRepo` returns a `*testutil.TestRepo` whose `CommitFile(filename, content, msg string) string` returns the new SHA, and `CheckoutDetached(start ...string)` detaches HEAD.

```go
func TestHandleEnqueueDetachedHeadInfersBranch(t *testing.T) {
	enqueue := func(t *testing.T, server *Server, reqData EnqueueRequest) *httptest.ResponseRecorder {
		t.Helper()
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		return w
	}

	t.Run("single commit attributes to nearest ancestor branch", func(t *testing.T) {
		server, _, tmpDir := newTestServer(t)
		repoDir := filepath.Join(tmpDir, "testrepo")
		repo := testutil.InitTestGitRepo(t, repoDir)

		repo.CheckoutNewBranch("feature-x")
		repo.CommitFile("f.txt", "content", "feature commit")
		repo.CheckoutDetached()
		repo.CommitFile("g.txt", "content", "detached commit")

		w := enqueue(t, server, EnqueueRequest{
			RepoPath: repoDir, GitRef: "HEAD", Agent: "test",
		})
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		var job storage.ReviewJob
		testutil.DecodeJSON(t, w, &job)
		assert.Equal(t, "feature-x", job.Branch)
	})

	t.Run("ambiguous tie stores empty branch", func(t *testing.T) {
		server, _, tmpDir := newTestServer(t)
		repoDir := filepath.Join(tmpDir, "testrepo")
		repo := testutil.InitTestGitRepo(t, repoDir)

		repo.CheckoutNewBranch("feature-a")
		repo.CommitFile("f.txt", "content", "shared tip")
		repo.RunGit("branch", "feature-b")
		repo.CheckoutDetached()
		repo.CommitFile("g.txt", "content", "detached commit")

		w := enqueue(t, server, EnqueueRequest{
			RepoPath: repoDir, GitRef: "HEAD", Agent: "test",
		})
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		var job storage.ReviewJob
		testutil.DecodeJSON(t, w, &job)
		assert.Empty(t, job.Branch)
	})

	t.Run("dirty review attributes via frozen HEAD", func(t *testing.T) {
		server, _, tmpDir := newTestServer(t)
		repoDir := filepath.Join(tmpDir, "testrepo")
		repo := testutil.InitTestGitRepo(t, repoDir)

		repo.CheckoutNewBranch("feature-x")
		repo.CommitFile("f.txt", "content", "feature commit")
		repo.CheckoutDetached()
		repo.CommitFile("g.txt", "content", "detached commit")

		w := enqueue(t, server, EnqueueRequest{
			RepoPath: repoDir, GitRef: "dirty", Agent: "test",
			DiffContent: "diff --git a/h.txt b/h.txt\n+new line\n",
		})
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		var job storage.ReviewJob
		testutil.DecodeJSON(t, w, &job)
		assert.Equal(t, "feature-x", job.Branch)
	})

	t.Run("inferred branch is subject to exclusion", func(t *testing.T) {
		server, db, tmpDir := newTestServer(t)
		repoDir := filepath.Join(tmpDir, "testrepo")
		repo := testutil.InitTestGitRepo(t, repoDir)

		repo.CheckoutNewBranch("wip-feature")
		repo.CommitFile("f.txt", "content", "feature commit")
		repo.CheckoutDetached()
		repo.CommitFile("g.txt", "content", "detached commit")
		require.NoError(t, os.WriteFile(
			filepath.Join(repoDir, ".roborev.toml"),
			[]byte(`excluded_branches = ["wip-feature"]`), 0o644,
		))

		w := enqueue(t, server, EnqueueRequest{
			RepoPath: repoDir, GitRef: "HEAD", Agent: "test",
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var response struct {
			Skipped bool   `json:"skipped"`
			Reason  string `json:"reason"`
		}
		testutil.DecodeJSON(t, w, &response)
		assert.True(t, response.Skipped)
		assert.Contains(t, response.Reason, "wip-feature")

		queued, _, _, _, _, _, _, _, _ := db.GetJobCounts()
		assert.Zero(t, queued, "no job should be enqueued for excluded inferred branch")
	})

	t.Run("client-sent branch wins over inference", func(t *testing.T) {
		server, _, tmpDir := newTestServer(t)
		repoDir := filepath.Join(tmpDir, "testrepo")
		repo := testutil.InitTestGitRepo(t, repoDir)

		repo.CheckoutNewBranch("feature-x")
		repo.CommitFile("f.txt", "content", "feature commit")
		repo.CheckoutDetached()
		repo.CommitFile("g.txt", "content", "detached commit")

		w := enqueue(t, server, EnqueueRequest{
			RepoPath: repoDir, GitRef: "HEAD", Agent: "test",
			Branch: "explicit-branch",
		})
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		var job storage.ReviewJob
		testutil.DecodeJSON(t, w, &job)
		assert.Equal(t, "explicit-branch", job.Branch)
	})
}
```

If `repo.RunGit` does not exist on `*testutil.TestRepo` with that signature, use `repo.Run("branch", "feature-b")` (check `internal/testutil/git.go:173` — `Run` returns output, `RunGit` at :235 discards it; either works).

- [ ] **Step 2: Run tests to verify the new behavior fails**

Run: `go test ./internal/daemon/ -run TestHandleEnqueueDetachedHeadInfersBranch -v`
Expected: FAIL — "single commit", "dirty review", and "inferred branch is subject to exclusion" subtests fail (branch stored as `""`, exclusion not applied). "ambiguous tie" and "client-sent branch" pass (they assert current behavior is preserved).

- [ ] **Step 3: Implement the call site**

In `internal/daemon/server.go`, `humaEnqueue`, directly after:

```go
	if early != nil {
		return early, nil
	}
```

(the `buildTargetDescriptor` early-return, ~line 2061-2063) and before the `merged := config.MergedReviewConfig(...)` line, insert:

```go
	// Detached-HEAD attribution: when the client sent no branch and HEAD has
	// no symbolic ref, infer the branch from the frozen target SHA so the
	// job lands in branch-scoped views. sessionSHA is the SHA the freeze
	// already resolved (single commit, range end, or dirty HEAD); reusing it
	// keeps inference and storage on the same commit. Prompt jobs have no
	// sessionSHA and are never attributed.
	if req.Branch == "" && req.JobType != storage.JobTypeInsights &&
		descriptor.sessionSHA != "" {
		if inferred := git.InferBranchForCommit(
			ctx, checkoutRoot, descriptor.sessionSHA,
		); inferred != "" {
			if config.IsBranchExcluded(checkoutRoot, inferred) {
				return rawJSONOutput(http.StatusOK, EnqueueSkippedResponse{
					Skipped: true,
					Reason: fmt.Sprintf(
						"branch %q is excluded from reviews", inferred,
					),
				})
			}
			log.Printf(
				"enqueue: inferred branch %q for detached-HEAD target %s",
				inferred, descriptor.sessionSHA,
			)
			descriptor.branch = inferred
			req.Branch = inferred
		}
	}
```

Both assignments matter: `descriptor.branch` is what `baseOpts()` stores on the job (the descriptor was frozen from the pre-inference request), and `req.Branch` feeds session reuse (`in.req.Branch` at ~server.go:2173) and the panel path's request copy.

Note `req.Branch == ""` here implies HEAD was detached: line ~1981 already set `req.Branch = currentBranch` for non-insights jobs, so a symbolic HEAD makes it non-empty.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestHandleEnqueueDetachedHeadInfersBranch -v`
Expected: PASS (all 5 subtests)

Regression check: `go test ./internal/daemon/`
Expected: ok

- [ ] **Step 5: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add -A
git commit -m "Infer branch for detached-HEAD enqueues"
```

---

### Task 3: Document the behavior

**Files:**
- Modify: `docs/guides/repository-management.md` (Git Worktrees section, ~line 119-127)

**Interfaces:**
- Consumes: behavior implemented in Tasks 1-2.
- Produces: user-facing docs only.

- [ ] **Step 1: Add the detached-HEAD paragraph**

In `docs/guides/repository-management.md`, the "Git Worktrees" section lists bullets under "When running commands from a worktree:". After that bullet list (after the `refine` bullet, ~line 126) and before the "Without this, ..." paragraph, insert:

```markdown
### Detached HEAD worktrees

Some tools (agent sandboxes, spec-driven development harnesses) create worktrees with a detached HEAD, where git reports no current branch. When a commit is made on detached HEAD, roborev infers the branch for the review: if exactly one local branch points at the commit it uses that, otherwise it picks the unique nearest branch whose tip is an ancestor of the commit — the usual shape when a tool's worktree runs ahead of the branch it mirrors. If no single best candidate exists (ties, or more than 20 candidate branches), the review is stored without a branch, as before. Inferred branches respect `excluded_branches`.
```

- [ ] **Step 2: Verify and commit**

Re-read the section for flow; the surrounding "Without this..." paragraph should still read naturally (it refers to main-repo consolidation, not branch inference — if it reads as contradicting, place the new content as a `###` subsection heading as shown, which separates it).

```bash
git add docs/guides/repository-management.md
git commit -m "Document detached-HEAD branch inference"
```

---

## Self-Review Notes

- Spec coverage: helper + algorithm (Task 1), post-freeze call site with sessionSHA reuse, exclusion skip, logging, req.Branch/session-reuse threading (Task 2), out-of-scope items untouched. Fail-closed cap and the >20 tests included (Task 1 steps 1/3). Dirty attribution and prompt-job skip covered (Task 2 tests + sessionSHA guard).
- Types: `InferBranchForCommit(ctx context.Context, repoPath, sha string) string` consistent across tasks; `descriptor.sessionSHA`/`descriptor.branch` match `targetDescriptor` at panel_enqueue.go:27.
- The plan and spec live under `docs/superpowers/`, which is deleted wholesale before the PR (explicit user instruction; handled outside these tasks).
