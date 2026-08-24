package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/githook"
)

func executePostCommitCmd(
	args ...string,
) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := postCommitCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func executeEnqueueAliasCmd(
	args ...string,
) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := enqueueCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestPostCommitSubmitsHEAD(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)

	repo.CommitFile("file.txt", "content", "initial commit")

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, "HEAD", req.GitRef)
}

func TestPostCommitBranchReview(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)

	repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
	repo.CommitFile("file.txt", "content", "initial")
	mainSHA := repo.Run("rev-parse", "HEAD")
	repo.Run("checkout", "-b", "feature")
	repo.CommitFile("feature.txt", "feature", "feature commit")
	writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	want := mainSHA + "..HEAD"
	assert.Equal(t, want, req.GitRef)
}

func TestPostCommitFallsBackOnBaseBranch(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)

	repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
	repo.CommitFile("file.txt", "content", "initial")
	writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, "HEAD", req.GitRef)
}

func TestPostCommitBatchSkipsUntilThreshold(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	requests := make(chan daemon.EnqueueRequest, 10)
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		requests <- req
		w.WriteHeader(http.StatusCreated)
	})

	repo.CommitFile("base.txt", "base", "base")
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	for i := 1; i <= 4; i++ {
		repo.CommitFile(
			fmt.Sprintf("change-%d.txt", i), "change", "change",
		)
		_, _, err := executePostCommitCmd("--repo", repo.Dir)
		require.NoError(t, err)
	}
	assert.Empty(t, requests, "below-threshold commits must not enqueue")

	head := repo.CommitFile("change-5.txt", "change", "change")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	req := <-requests
	base := repo.Run("rev-parse", head+"~5")
	assert.Equal(t, base+".."+head, req.GitRef)
	assert.Empty(t, requests, "batch must enqueue exactly once")
}

func TestPostCommitBatchEnqueuesPlannedBranchDespiteConcurrentSwitch(
	t *testing.T,
) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 2`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	head := repo.CommitFile("two.txt", "two", "two")

	oldHook := testHookAfterBatchPlan
	t.Cleanup(func() { testHookAfterBatchPlan = oldHook })
	testHookAfterBatchPlan = func() {
		testHookAfterBatchPlan = oldHook
		repo.CheckoutNewBranch("other")
		repo.CommitFile("three.txt", "three", "three")
	}
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "feature", req.Branch)
	assert.Equal(t, base+".."+head, req.GitRef)
}

func TestPostCommitBatchBranchModeKeepsBranchRange(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "feature")
	writeRoborevConfig(t, repo, `
post_commit_batch_size = 2
post_commit_review = "branch"
`)

	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	head := repo.CommitFile("two.txt", "two", "two")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, base+".."+head, req.GitRef)
}

func TestPostCommitBatchBranchFallbackKeepsAccumulatedRange(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
	base := repo.CommitFile("base.txt", "base", "base")
	writeRoborevConfig(t, repo, `
post_commit_batch_size = 2
post_commit_review = "branch"
`)

	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	head := repo.CommitFile("two.txt", "two", "two")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, base+".."+head, req.GitRef)
}

func TestPostCommitBatchFailedEnqueueRetriesRange(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	requests := make(chan daemon.EnqueueRequest, 2)
	var calls atomic.Int32
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		requests <- req
		if calls.Add(1) == 1 {
			http.Error(w, "temporary failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})

	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "feature")
	writeRoborevConfig(t, repo, `post_commit_batch_size = 2`)
	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.CommitFile("two.txt", "two", "two")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	failed := <-requests

	head := repo.CommitFile("three.txt", "three", "three")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	retried := <-requests

	assert.Equal(t, base+".."+repo.Run("rev-parse", head+"^1"), failed.GitRef)
	assert.Equal(t, base+".."+head, retried.GitRef)
}

func TestPostCommitBatchSerializesConcurrentHooks(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 2`)
	var requests atomic.Int32
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		w.WriteHeader(http.StatusCreated)
	})

	repo.CommitFile("base.txt", "base", "base")
	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.CommitFile("two.txt", "two", "two")

	errCh := make(chan error, 2)
	go func() {
		_, _, err := executePostCommitCmd("--repo", repo.Dir)
		errCh <- err
	}()
	<-firstStarted
	go func() {
		_, _, err := executePostCommitCmd("--repo", repo.Dir)
		errCh <- err
	}()

	serialized := assert.Never(t, func() bool {
		return requests.Load() > 1
	}, 200*time.Millisecond, 10*time.Millisecond)
	close(releaseFirst)
	require.NoError(t, <-errCh)
	require.NoError(t, <-errCh)
	assert.True(t, serialized)
	assert.Equal(t, int32(1), requests.Load())
}

func TestPostCommitLockTimeoutFailsOpenToImmediateReview(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	repo.CommitFile("file.txt", "content", "initial")
	unlock, err := acquirePostCommitBatchLock(t.Context(), repo.Dir)
	require.NoError(t, err)
	defer func() { _ = unlock() }()
	old := postCommitBatchLockTimeout
	postCommitBatchLockTimeout = 50 * time.Millisecond
	t.Cleanup(func() { postCommitBatchLockTimeout = old })

	// In immediate mode nothing retries a skipped commit, so a lock timeout
	// must fail open to a single-commit review instead of dropping it. No
	// batch state is read or written without the lock.
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "HEAD", req.GitRef)
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	_, err = os.Stat(statePath)
	assert.ErrorIs(t, err, os.ErrNotExist,
		"the lockless fallback must not touch batch state")
}

func TestPostCommitBatchFlushesPendingCommits(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	head := repo.CommitFile("two.txt", "two", "two")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	_, _, err = executePostCommitCmd("--repo", repo.Dir, "--flush")
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, base+".."+head, req.GitRef)
}

func TestPostCommitBatchFlushPushUsesTargetBranchWorktree(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	base := repo.CommitFile(
		".roborev.toml", "post_commit_batch_size = 5\n", "config",
	)
	worktreeDir := t.TempDir()
	resolvedWorktree, err := filepath.EvalSymlinks(worktreeDir)
	require.NoError(t, err)
	repo.Run("worktree", "add", resolvedWorktree, "-b", "feature")
	worktree := &TestGitRepo{Dir: resolvedWorktree, t: t}
	head := worktree.CommitFile("feature.txt", "feature", "feature")
	_, _, err = executePostCommitCmd("--repo", worktree.Dir)
	require.NoError(t, err)

	input := strings.NewReader(fmt.Sprintf(
		"refs/heads/feature %s refs/heads/feature %s\n",
		head, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, resolvedWorktree, req.RepoPath)
	assert.Equal(t, base+".."+head, req.GitRef)
}

func TestPostCommitBatchFlushPushFlushesBranchWithoutWorktree(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	base := repo.CommitFile(
		".roborev.toml", "post_commit_batch_size = 5\n", "config",
	)
	mainBranch := repo.Run("branch", "--show-current")
	repo.Run("checkout", "-b", "feature")
	head := repo.CommitFile("feature.txt", "feature", "feature")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.Run("checkout", mainBranch)

	// Pushing a branch no worktree has checked out must still flush its
	// pending range (from the pushing worktree), or its commits leave the
	// machine unreviewed with no later event to catch them.
	input := strings.NewReader(fmt.Sprintf(
		"refs/heads/feature %s refs/heads/feature %s\n",
		head, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "feature", req.Branch)
	assert.Equal(t, base+".."+head, req.GitRef)
	assert.Equal(t, repo.Dir, req.RepoPath)
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, head, state.Branches["feature"].Checkpoint)
}

func TestPostCommitBatchFlushBranchIgnoresRebaseInProgress(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "feature")
	head := repo.CommitFile("feature.txt", "feature", "feature")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	// A rebase in the worktree handling the flush must not drop it: the
	// flush carries an explicit branch and pushed SHA, and skipping it would
	// let pending commits be pushed unreviewed.
	gitDir := strings.TrimSpace(repo.Run("rev-parse", "--git-dir"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repo.Dir, gitDir)
	}
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "rebase-merge"), 0o755))

	_, _, err = executePostCommitCmd(
		"--repo", repo.Dir, "--flush",
		"--flush-branch", "feature", "--flush-head", head,
	)
	require.NoError(t, err)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "feature", req.Branch)
	assert.Equal(t, base+".."+head, req.GitRef)
}

func TestPostCommitBatchFlushPushCorruptStateFailsOpen(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "feature")
	head := repo.CommitFile("feature.txt", "feature", "feature")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	require.NoError(t, os.WriteFile(statePath, []byte("not json\n"), 0o600))
	input := strings.NewReader(fmt.Sprintf(
		"refs/heads/feature %s refs/heads/feature %s\n",
		head, strings.Repeat("0", 40),
	))

	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "feature", req.Branch)
	assert.Equal(t, head, req.GitRef)
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, head, state.Branches["feature"].Checkpoint)
}

func TestPostCommitBatchFlushPushRecoversOffChainCheckpoint(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "feature")
	repo.CommitFile("one.txt", "one", "one")
	head := repo.CommitFile("two.txt", "two", "two")
	repo.Run("checkout", "-b", "side", base)
	side := repo.CommitFile("side.txt", "side", "side")
	repo.Run("checkout", "feature")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	require.NoError(t, savePostCommitBatchState(statePath, postCommitBatchState{
		Version: postCommitBatchStateVersion,
		Branches: map[string]postCommitBatchEntry{
			"feature": {Checkpoint: side},
		},
	}))
	input := strings.NewReader(fmt.Sprintf(
		"refs/heads/feature %s refs/heads/feature %s\n",
		head, strings.Repeat("0", 40),
	))

	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "feature", req.Branch)
	assert.Equal(t, base+".."+head, req.GitRef)
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, head, state.Branches["feature"].Checkpoint)
}

func TestPostCommitBatchFlushPushUsesExactPushedSHA(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "feature")
	pushedHead := repo.CommitFile("pushed.txt", "pushed", "pushed")
	later := repo.CommitFile("later.txt", "later", "later")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	// The recorded tip is ahead of the pushed SHA; planning at the older
	// boundary must not treat the range as having left the branch.
	require.NoError(t, savePostCommitBatchState(statePath, postCommitBatchState{
		Version: postCommitBatchStateVersion,
		Branches: map[string]postCommitBatchEntry{
			"feature": {Checkpoint: base, Tip: later},
		},
	}))
	input := strings.NewReader(fmt.Sprintf(
		"refs/heads/feature %s refs/heads/feature %s\n",
		pushedHead, strings.Repeat("0", 40),
	))

	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "feature", req.Branch)
	assert.Equal(t, base+".."+pushedHead, req.GitRef)
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, pushedHead, state.Branches["feature"].Checkpoint)
}

func TestPostCommitBatchFlushPushTagCarriesRenamedBranchRange(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "work")
	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	head := repo.CommitFile("two.txt", "two", "two")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.Run("branch", "-m", "work", "renamed")

	// Pushing a tag at the old tip carries the renamed branch's pending
	// commits even though no refs/heads ref is updated; the orphaned entry's
	// recorded range must still flush them before they leave the machine.
	repo.Run("tag", "v1", head)
	input := strings.NewReader(fmt.Sprintf(
		"refs/tags/v1 %s refs/tags/v1 %s\n",
		head, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "work", req.Branch)
	assert.Equal(t, base+".."+head, req.GitRef)
}

func TestPostCommitBatchPartialFlushKeepsRemainderFlushable(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	requests := make(chan daemon.EnqueueRequest, 3)
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		requests <- req
		w.WriteHeader(http.StatusCreated)
	})
	base := repo.CommitFile(
		".roborev.toml", "post_commit_batch_size = 5\n", "config",
	)
	repo.Run("checkout", "-b", "parent")
	pOne := repo.CommitFile("p1.txt", "p1", "p1")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	pTwo := repo.CommitFile("p2.txt", "p2", "p2")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.Run("checkout", "-b", "child", pOne)
	childHead := repo.CommitFile("c1.txt", "c1", "c1")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.Run("branch", "-m", "parent", "moved")

	// The child push flushes the orphaned range only through the fork point;
	// the flush must keep the recorded tip so the remainder stays pending.
	input := strings.NewReader(fmt.Sprintf(
		"refs/heads/child %s refs/heads/child %s\n",
		childHead, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)
	require.Len(t, requests, 2)
	got := map[string]string{}
	for range 2 {
		req := <-requests
		got[req.Branch] = req.GitRef
	}
	require.Equal(t, map[string]string{
		"child":  pOne + ".." + childHead,
		"parent": base + ".." + pOne,
	}, got)

	// A tag push at the original tip carries the unreviewed remainder.
	repo.Run("tag", "v1", pTwo)
	input = strings.NewReader(fmt.Sprintf(
		"refs/tags/v1 %s refs/tags/v1 %s\n",
		pTwo, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, requests, 1)
	req := <-requests
	assert.Equal(t, "parent", req.Branch)
	assert.Equal(t, pOne+".."+pTwo, req.GitRef)
}

func TestPostCommitBatchFlushPushSideBranchDoesNotCorruptParent(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	requests := make(chan daemon.EnqueueRequest, 3)
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		requests <- req
		w.WriteHeader(http.StatusCreated)
	})
	base := repo.CommitFile(
		".roborev.toml", "post_commit_batch_size = 5\n", "config",
	)
	repo.Run("checkout", "-b", "side")
	sideHead := repo.CommitFile("s1.txt", "s1", "s1")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.Run("checkout", "-b", "parent", base)
	repo.Run("merge", "--no-ff", "-m", "merge side", "side")
	mergeHead := repo.Run("rev-parse", "HEAD")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	// The merge base of the parent tip and the pushed side head is side
	// history, reachable from the parent only through the merge's second
	// parent. Flushing the parent there would advance its checkpoint off its
	// first-parent chain, stranding the merge commit once the branch is
	// renamed.
	input := strings.NewReader(fmt.Sprintf(
		"refs/heads/side %s refs/heads/side %s\n",
		sideHead, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)
	require.Len(t, requests, 1)
	req := <-requests
	require.Equal(t, "side", req.Branch)
	require.Equal(t, base+".."+sideHead, req.GitRef)
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, base, state.Branches["parent"].Checkpoint,
		"a side-branch push must not move the parent's checkpoint")

	repo.Run("branch", "-m", "parent", "moved")
	input = strings.NewReader(fmt.Sprintf(
		"refs/heads/moved %s refs/heads/moved %s\n",
		mergeHead, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, requests, 1)
	req = <-requests
	assert.Equal(t, "moved", req.Branch)
	assert.Equal(t, base+".."+mergeHead, req.GitRef,
		"the renamed branch must still flush the merge commit")
}

func TestPostCommitBatchFlushPushTagCarriesDivergedRecordedRange(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "work")
	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	oldTip := repo.CommitFile("two.txt", "two", "two")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	// The branch is force-moved onto divergent history while its recorded
	// range is still pending. Pushing a tag at the old tip carries those
	// commits; the flush must consult the recorded range, not just the live
	// ref, or they leave the machine unreviewed.
	repo.Run("checkout", "-b", "other", base)
	otherHead := repo.CommitFile("other.txt", "other", "other")
	repo.Run("branch", "-f", "work", otherHead)
	repo.Run("tag", "v1", oldTip)
	input := strings.NewReader(fmt.Sprintf(
		"refs/tags/v1 %s refs/tags/v1 %s\n",
		oldTip, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "work", req.Branch)
	assert.Equal(t, base+".."+oldTip, req.GitRef)
}

func TestPostCommitBatchFlushPushTagCarriesOrphanHistoryRange(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "work")
	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	oldTip := repo.CommitFile("two.txt", "two", "two")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	// The branch is force-moved onto an unrelated root. Unlike related
	// divergence, first-parent distance to the live tip errors instead of
	// answering off-chain; the recorded range is abandoned history either
	// way and a tag push of its tip must still flush it.
	repo.Run("checkout", "--orphan", "fresh")
	freshHead := repo.CommitFile("fresh.txt", "fresh", "fresh")
	repo.Run("branch", "-f", "work", freshHead)
	repo.Run("tag", "v1", oldTip)
	input := strings.NewReader(fmt.Sprintf(
		"refs/tags/v1 %s refs/tags/v1 %s\n",
		oldTip, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "work", req.Branch)
	assert.Equal(t, base+".."+oldTip, req.GitRef)
}

func TestPostCommitBatchFlushPushKeepsBranchReviewRange(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "feature")
	writeRoborevConfig(t, repo, `
post_commit_batch_size = 2
post_commit_review = "branch"
`)

	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.CommitFile("two.txt", "two", "two")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	<-reqCh

	head := repo.CommitFile("three.txt", "three", "three")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	input := strings.NewReader(fmt.Sprintf(
		"refs/heads/feature %s refs/heads/feature %s\n",
		head, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, base+".."+head, req.GitRef)
}

func TestPostCommitBatchFlushPushResolvesHEADSource(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "feature")
	head := repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	input := strings.NewReader(fmt.Sprintf(
		"HEAD %s refs/heads/published %s\n",
		head, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "feature", req.Branch)
	assert.Equal(t, base+".."+head, req.GitRef)
}

func TestPostCommitBatchFlushPushUsesNonBranchSourceAsAncestor(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "feature")
	head := repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	input := strings.NewReader(fmt.Sprintf(
		"HEAD~0 %s refs/heads/published %s\n",
		head, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "feature", req.Branch)
	assert.Equal(t, base+".."+head, req.GitRef)
}

func TestPostCommitBatchFlushPushMigratesRenamedBranch(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "old-name")
	head := repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.Run("branch", "-m", "new-name")

	input := strings.NewReader(fmt.Sprintf(
		"refs/heads/new-name %s refs/heads/new-name %s\n",
		head, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "new-name", req.Branch)
	assert.Equal(t, base+".."+head, req.GitRef)
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.NotContains(t, state.Branches, "old-name")
	assert.Equal(t, head, state.Branches["new-name"].Checkpoint)
}

func TestPostCommitBatchFlushPushFlushesAncestorBranchRanges(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	requests := make(chan daemon.EnqueueRequest, 2)
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		requests <- req
		w.WriteHeader(http.StatusCreated)
	})
	base := repo.CommitFile(
		".roborev.toml", "post_commit_batch_size = 5\n", "config",
	)
	repo.Run("checkout", "-b", "parent")
	parentTip := repo.CommitFile("parent.txt", "parent", "parent")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.Run("checkout", "-b", "child")
	childHead := repo.CommitFile("child.txt", "child", "child")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	parentWorktree := t.TempDir()
	repo.Run("worktree", "add", parentWorktree, "parent")

	input := strings.NewReader(fmt.Sprintf(
		"refs/heads/child %s refs/heads/child %s\n",
		childHead, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, requests, 2)
	got := map[string]string{}
	for range 2 {
		req := <-requests
		got[req.Branch] = req.GitRef
	}
	assert.Equal(t, map[string]string{
		"child":  parentTip + ".." + childHead,
		"parent": base + ".." + parentTip,
	}, got)
}

func TestPostCommitBatchFlushPushFlushesPartiallyCarriedParentRange(
	t *testing.T,
) {
	repo, mux := setupTestEnvironment(t)
	requests := make(chan daemon.EnqueueRequest, 2)
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		requests <- req
		w.WriteHeader(http.StatusCreated)
	})
	base := repo.CommitFile(
		".roborev.toml", "post_commit_batch_size = 5\n", "config",
	)
	repo.Run("checkout", "-b", "parent")
	pOne := repo.CommitFile("p1.txt", "p1", "p1")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.CommitFile("p2.txt", "p2", "p2")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	// The child branches partway into the parent's pending range: pushing
	// it carries p1 upstream while p2 stays local and pending.
	repo.Run("checkout", "-b", "child", pOne)
	childHead := repo.CommitFile("child.txt", "child", "child")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	parentWorktree := t.TempDir()
	repo.Run("worktree", "add", parentWorktree, "parent")
	input := strings.NewReader(fmt.Sprintf(
		"refs/heads/child %s refs/heads/child %s\n",
		childHead, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, requests, 2)
	got := map[string]string{}
	for range 2 {
		req := <-requests
		got[req.Branch] = req.GitRef
	}
	assert.Equal(t, map[string]string{
		"child":  pOne + ".." + childHead,
		"parent": base + ".." + pOne,
	}, got)
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, pOne, state.Branches["parent"].Checkpoint)
}

func TestPostCommitBatchDisableDrainsRenamedBranchRange(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "old")
	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.Run("branch", "-m", "old", "new")
	head := repo.CommitFile("two.txt", "two", "two")

	writeRoborevConfig(t, repo, `post_commit_batch_size = 1`)
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, base+".."+head, req.GitRef,
		"disabling batching after a rename must drain the pre-rename range")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.NotContains(t, state.Branches, "old")
	assert.NotContains(t, state.Branches, "new")
}

func TestPostCommitBatchFlushPushMigratesConsecutiveRenames(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueueCapture(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.Run("checkout", "-b", "old")
	head := repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.Run("branch", "-m", "old", "middle")
	repo.Run("branch", "-m", "middle", "new")

	input := strings.NewReader(fmt.Sprintf(
		"refs/heads/new %s refs/heads/new %s\n",
		head, strings.Repeat("0", 40),
	))
	flushPushedPostCommitBatches(t.Context(), repo.Dir, input)

	require.Len(t, reqCh, 1)
	req := <-reqCh
	assert.Equal(t, "new", req.Branch)
	assert.Equal(t, base+".."+head, req.GitRef)
}

func TestPostCommitBatchDisableDrainsPendingRange(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	head := repo.CommitFile("two.txt", "two", "two")

	writeRoborevConfig(t, repo, `post_commit_batch_size = 1`)
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, base+".."+head, req.GitRef)

	repo.CommitFile("three.txt", "three", "three")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	assert.Equal(t, "HEAD", (<-reqCh).GitRef)
}

func TestPostCommitBatchConfigErrorPreservesPendingState(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	requests := make(chan daemon.EnqueueRequest, 1)
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		requests <- req
		w.WriteHeader(http.StatusCreated)
	})
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	repo.CommitFile("base.txt", "base", "base")
	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	path, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	before, _, err := loadPostCommitBatchState(path)
	require.NoError(t, err)

	writeRoborevConfig(t, repo, `post_commit_batch_size = `)
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	after, _, err := loadPostCommitBatchState(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Empty(t, requests)
}

func TestPostCommitBatchLogsPendingCount(t *testing.T) {
	repo, _ := setupTestEnvironment(t)
	repo.CommitFile("base.txt", "base", "base")
	writeRoborevConfig(t, repo, `post_commit_batch_size = 5`)
	logFile := filepath.Join(t.TempDir(), "post-commit.log")
	old := hookLogPath
	hookLogPath = logFile
	t.Cleanup(func() { hookLogPath = old })
	repo.CommitFile("one.txt", "one", "one")

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"outcome":"skip"`)
	assert.Contains(t, string(data), "commits=1 threshold=5")
}

func TestPostCommitSilentExitNotARepo(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr, err := executePostCommitCmd("--repo", dir)
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
}

func TestPostCommitAcceptsQuietFlag(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	mockEnqueue(t, mux)

	repo.CommitFile("file.txt", "content", "initial")

	_, _, err := executePostCommitCmd(
		"--repo", repo.Dir, "--quiet",
	)
	require.NoError(t, err)
}

func TestEnqueueAliasWorksIdentically(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)

	repo.CommitFile("file.txt", "content", "initial")

	_, _, err := executeEnqueueAliasCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, "HEAD", req.GitRef)
}

func TestPostCommitRejectsPositionalArgs(t *testing.T) {
	_, _, err := executePostCommitCmd("abc123")
	require.Error(t, err)

	assert.Contains(t, err.Error(), "unknown command")
}

func TestEnqueueRejectsPositionalArgs(t *testing.T) {
	_, _, err := executeEnqueueAliasCmd("abc123")
	require.Error(t, err)
}

func TestPostCommitLogsSuccess(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	mockEnqueue(t, mux)

	logFile := filepath.Join(t.TempDir(), "post-commit.log")
	old := hookLogPath
	hookLogPath = logFile
	t.Cleanup(func() { hookLogPath = old })

	repo.CommitFile("file.txt", "content", "initial commit")

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"outcome":"ok"`)
	assert.Contains(t, string(data), `"repo"`)
}

func TestPostCommitLogsSkipNotARepo(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "post-commit.log")
	old := hookLogPath
	hookLogPath = logFile
	t.Cleanup(func() { hookLogPath = old })

	dir := t.TempDir()
	_, _, err := executePostCommitCmd("--repo", dir)
	require.NoError(t, err)

	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"outcome":"skip"`)
	assert.Contains(t, string(data), "not a git repo")
}

func TestPostCommitLogsSkipRebase(t *testing.T) {
	repo, _ := setupTestEnvironment(t)
	repo.CommitFile("file.txt", "content", "initial commit")

	logFile := filepath.Join(t.TempDir(), "post-commit.log")
	old := hookLogPath
	hookLogPath = logFile
	t.Cleanup(func() { hookLogPath = old })

	// Simulate rebase in progress.
	gitDir := strings.TrimSpace(repo.Run("rev-parse", "--git-dir"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repo.Dir, gitDir)
	}
	require.NoError(t, os.MkdirAll(
		filepath.Join(gitDir, "rebase-merge"), 0o755))

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"outcome":"skip"`)
	assert.Contains(t, string(data), "rebase in progress")
}

func TestPostCommitLogsDaemonFailure(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "post-commit.log")
	old := hookLogPath
	hookLogPath = logFile
	t.Cleanup(func() { hookLogPath = old })

	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial")

	// Point serverAddr at nothing so ensureDaemon fails.
	patchServerAddr(t, "http://127.0.0.1:1")

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"outcome":"fail"`)
	assert.Contains(t, string(data), "daemon")
}

func TestPostCommitLogsCreatesParentDir(t *testing.T) {
	// Log path under a directory that doesn't exist yet,
	// simulating a fresh install where ~/.roborev is absent.
	logFile := filepath.Join(t.TempDir(), "subdir", "post-commit.log")
	old := hookLogPath
	hookLogPath = logFile
	t.Cleanup(func() { hookLogPath = old })

	dir := t.TempDir()
	_, _, err := executePostCommitCmd("--repo", dir)
	require.NoError(t, err)

	data, err := os.ReadFile(logFile)
	require.NoError(t, err, "log file should be created even when parent dir is absent")
	assert.Contains(t, string(data), `"outcome":"skip"`)
}

// stallingRoundTripper blocks until the request context is
// cancelled, then returns an error. This simulates a daemon
// that accepts connections but never responds, without needing
// a real httptest server or a long sleep.
type stallingRoundTripper struct {
	hit       chan struct{}
	cancelled chan time.Duration
}

func (s *stallingRoundTripper) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	start := time.Now()
	select {
	case s.hit <- struct{}{}:
	default:
	}
	select {
	case <-req.Context().Done():
		select {
		case s.cancelled <- time.Since(start):
		default:
		}
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("stallingRoundTripper: context was never cancelled")
	}
	return nil, fmt.Errorf("request cancelled: %w", req.Context().Err())
}

func TestPostCommitTimesOutOnSlowDaemon(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	// Register a handler so ensureDaemon succeeds, but the
	// actual POST will go through the stalling RoundTripper.
	realHandlerCalled := false
	mux.HandleFunc("/api/enqueue", func(
		w http.ResponseWriter, r *http.Request,
	) {
		realHandlerCalled = true
	})

	repo.CommitFile("file.txt", "content", "initial")

	rt := &stallingRoundTripper{
		hit:       make(chan struct{}, 1),
		cancelled: make(chan time.Duration, 1),
	}
	orig := hookHTTPClient
	hookHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{
			Timeout:   50 * time.Millisecond,
			Transport: rt,
		}
	}
	t.Cleanup(func() { hookHTTPClient = orig })

	_, _, err := executePostCommitCmd("--repo", repo.Dir)

	require.NoError(t, err)

	select {
	case <-rt.hit:
		// RoundTrip was called — timeout path was exercised
	default:
		require.NoError(t, err, "RoundTrip was never called; timeout not exercised")
	}
	assert.False(t, realHandlerCalled, "real handler should not be reached")

	select {
	case elapsed := <-rt.cancelled:
		assert.LessOrEqual(t, elapsed, time.Second,
			"request took %v; should return promptly via timeout",
			elapsed)
	default:
		require.FailNow(t, "request context was not cancelled by timeout")
	}
}

func TestEnqueueAliasIsHidden(t *testing.T) {
	cmd := enqueueCmd()
	assert.True(t, cmd.Hidden)
	assert.Contains(t, cmd.Use, "enqueue")
}

// repoUnderTest holds a repo for post-commit hook tests.
type repoUnderTest struct {
	// repo is the directory post-commit runs from (may be a worktree).
	repo *TestGitRepo
}

// setupPlainRepo returns a repoUnderTest backed by a plain (non-worktree) repo.
func setupPlainRepo(t *testing.T) repoUnderTest {
	t.Helper()
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")
	return repoUnderTest{repo: repo}
}

// setupWorktreeRepo returns a repoUnderTest backed by a linked worktree.
func setupWorktreeRepo(t *testing.T) repoUnderTest {
	t.Helper()
	mainRepo := newTestGitRepo(t)
	mainRepo.CommitFile("file.txt", "content", "initial commit")

	wtDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(wtDir)
	require.NoError(t, err)
	mainRepo.Run("worktree", "add", resolved, "-b", "worktree-branch")

	return repoUnderTest{repo: &TestGitRepo{Dir: resolved, t: t}}
}

// mockEnqueueCapture registers a handler on mux that captures full enqueue
// requests. The returned channel receives at most one request.
func mockEnqueueCapture(t *testing.T, mux *http.ServeMux) <-chan daemon.EnqueueRequest {
	t.Helper()
	ch := make(chan daemon.EnqueueRequest, 1)
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case ch <- req:
		default:
			assert.Fail(t, "mockEnqueueCapture: unexpected extra request")
			http.Error(w, "duplicate request", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	return ch
}

// TestPostCommitSendsLocalRepoPath checks that the RepoPath in the enqueue
// request is the local (worktree) path in both plain repos and linked
// worktrees. The daemon canonicalizes to the main repo root itself.
func TestPostCommitSendsLocalRepoPath(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) repoUnderTest
	}{
		{"plain repo", setupPlainRepo},
		{"worktree", setupWorktreeRepo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.setup(t)
			mux := http.NewServeMux()
			daemonFromHandler(t, mux)
			reqCh := mockEnqueueCapture(t, mux)

			r.repo.CommitFile("change.txt", "content", "a commit")

			_, _, err := executePostCommitCmd("--repo", r.repo.Dir)
			require.NoError(t, err)

			req := <-reqCh
			assert.Equal(t, r.repo.Dir, req.RepoPath)
		})
	}
}

// TestPostCommitSkipsEnqueueDuringRebase exercises the real Go code path
// (postCommitCmd → git.IsRebaseInProgress) by simulating a rebase state
// with a synthetic rebase-merge directory. This is the unit-level
// complement to TestPostCommitDoesNotEnqueueDuringRebase which tests the
// end-to-end shell hook flow.
func TestPostCommitSkipsEnqueueDuringRebase(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T) repoUnderTest
		sentinel string // directory to create inside git dir
	}{
		{"plain repo/rebase-merge", setupPlainRepo, "rebase-merge"},
		{"plain repo/rebase-apply", setupPlainRepo, "rebase-apply"},
		{"worktree/rebase-merge", setupWorktreeRepo, "rebase-merge"},
		{"worktree/rebase-apply", setupWorktreeRepo, "rebase-apply"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.setup(t)
			mux := http.NewServeMux()
			daemonFromHandler(t, mux)

			mux.HandleFunc("/api/enqueue", func(
				w http.ResponseWriter, r *http.Request,
			) {
				t.Error("enqueue should not be called during rebase")
				http.Error(w, "unexpected", http.StatusConflict)
			})

			// Resolve the actual git dir (may differ from .git/ in
			// linked worktrees where .git is a file).
			gitDir := strings.TrimSpace(r.repo.Run(
				"rev-parse", "--git-dir"))
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(r.repo.Dir, gitDir)
			}
			require.NoError(t, os.MkdirAll(
				filepath.Join(gitDir, tt.sentinel), 0o755))

			_, _, err := executePostCommitCmd("--repo", r.repo.Dir)
			require.NoError(t, err)
		})
	}
}

// mockRoborevBinary creates a mock "roborev" shell script in a temp directory
// and returns the directory (to prepend to PATH). The mock script handles
// "post-commit" by using roborev's same rebase detection logic: it checks
// for rebase-merge/rebase-apply in git-dir and writes to the marker file
// only when NOT rebasing.
func mockRoborevBinary(t *testing.T, marker string) string {
	t.Helper()
	binDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
# Mock roborev binary for testing post-commit hook behavior.
# Only handles the "post-commit" subcommand.
case "$1" in
  post-commit)
    git_dir=$(git rev-parse --git-dir 2>/dev/null) || exit 0
    [ -d "$git_dir/rebase-merge" ] && exit 0
    [ -d "$git_dir/rebase-apply" ] && exit 0
    echo enqueued >> %q
    ;;
esac
`, marker)
	require.NoError(t, os.WriteFile(
		filepath.Join(binDir, "roborev"),
		[]byte(script), 0o755))
	return binDir
}

// installMockHook installs the real githook-generated post-commit hook with
// the ROBOREV= line patched to point at a mock binary.
func installMockHook(t *testing.T, repoDir, mockBinDir string) {
	t.Helper()
	hooksDir, err := gitrepo.HooksPath(context.Background(), repoDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(hooksDir, 0o755))

	hookContent := githook.GeneratePostCommit()
	mockBin := filepath.Join(mockBinDir, "roborev")
	lines := strings.Split(hookContent, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "ROBOREV=") {
			lines[i] = fmt.Sprintf("ROBOREV=%q", mockBin)
			break
		}
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(hooksDir, "post-commit"),
		[]byte(strings.Join(lines, "\n")), 0o755))
}

// TestPostCommitDoesNotEnqueueDuringRebase runs a real clean git rebase with
// hooks installed via githook.GeneratePostCommit and a mock roborev binary in
// PATH. It asserts that roborev's rebase detection prevents any enqueue during
// replayed commits.
//
// The mock binary reimplements the rebase guard in shell using the same
// git rev-parse --git-dir + rebase-merge/rebase-apply check that
// git.IsRebaseInProgress uses. TestPostCommitSkipsEnqueueDuringRebase (above)
// tests the real Go code path (postCommitCmd) via simulated rebase state; this
// test validates the end-to-end hook installation and invocation flow during
// an actual git rebase.
//
// The "hook before commits" variant installs the hook before the branch
// topology commits, so the hook fires for every setup commit as well. The
// "hook after commits" variant installs just before the rebase.
func TestPostCommitDoesNotEnqueueDuringRebase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell hooks and Unix PATH semantics")
	}
	tests := []struct {
		name            string
		setup           func(t *testing.T) repoUnderTest
		hookBeforeSetup bool
	}{
		{"plain repo/hook before commits", setupPlainRepo, true},
		{"plain repo/hook after commits", setupPlainRepo, false},
		{"worktree/hook before commits", setupWorktreeRepo, true},
		{"worktree/hook after commits", setupWorktreeRepo, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.setup(t)

			marker := filepath.Join(r.repo.Dir, "hook-enqueues.log")
			mockBinDir := mockRoborevBinary(t, marker)

			// Build env with mock roborev first in PATH so the hook finds it.
			gitEnv := append(os.Environ(),
				"PATH="+mockBinDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"HOME="+r.repo.Dir,
				"GIT_CONFIG_NOSYSTEM=1",
				"GIT_AUTHOR_NAME=Test",
				"GIT_AUTHOR_EMAIL=test@test.com",
				"GIT_COMMITTER_NAME=Test",
				"GIT_COMMITTER_EMAIL=test@test.com",
			)

			if tt.hookBeforeSetup {
				installMockHook(t, r.repo.Dir, mockBinDir)
			}

			// Create a branch topology for a clean rebase:
			//   base: A -- B (base.txt, no conflict)
			//          \
			//   current: C -- D -- E (branch files)
			gitCmd := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = r.repo.Dir
				cmd.Env = gitEnv
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "git %v failed: %s", args, out)
			}
			gitCmd("checkout", "-b", "rebase-base")
			gitCmd("commit", "--allow-empty", "-m", "base commit")
			gitCmd("checkout", "-")
			// Create 3 feature commits with actual file changes.
			for i := 1; i <= 3; i++ {
				f := filepath.Join(r.repo.Dir, fmt.Sprintf("branch%d.txt", i))
				require.NoError(t, os.WriteFile(f, fmt.Appendf(nil, "content %d", i), 0o644))
				gitCmd("add", f)
				gitCmd("commit", "-m", fmt.Sprintf("feature commit %d", i))
			}

			if !tt.hookBeforeSetup {
				installMockHook(t, r.repo.Dir, mockBinDir)
			}

			// Positive control: make a normal commit to prove the hook
			// fires outside of a rebase. Without this, a broken hook
			// install would silently pass (0 == 0).
			gitCmd("commit", "--allow-empty", "-m", "positive control commit")
			data, err := os.ReadFile(marker)
			require.NoError(t, err, "hook should have fired on normal commit")
			preRebaseCount := strings.Count(string(data), "enqueued")
			require.GreaterOrEqual(t, preRebaseCount, 1,
				"hook must fire at least once before rebase to prove it works")

			// Run a full clean rebase — all 3 feature commits replay.
			cmd := exec.Command("git", "rebase", "rebase-base")
			cmd.Dir = r.repo.Dir
			cmd.Env = gitEnv
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "rebase should succeed cleanly: %s", out)

			// After the rebase, the marker count should be unchanged.
			// If the hook enqueued during the rebase, there would be more.
			data, err = os.ReadFile(marker)
			require.NoError(t, err)
			postRebaseCount := strings.Count(string(data), "enqueued")
			assert.Equal(t, preRebaseCount, postRebaseCount,
				"hook should not have enqueued during rebase (got %d, want %d)",
				postRebaseCount, preRebaseCount)
		})
	}
}
