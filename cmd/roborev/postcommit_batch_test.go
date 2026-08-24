package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquirePostCommitBatchLockWaitsForHolder(t *testing.T) {
	type result struct {
		unlock func() error
		err    error
	}

	repo := newTestGitRepo(t)
	firstUnlock, err := acquirePostCommitBatchLock(t.Context(), repo.Dir)
	require.NoError(t, err)
	defer func() { _ = firstUnlock() }()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	started := make(chan struct{})
	acquired := make(chan result, 1)
	go func() {
		close(started)
		unlock, lockErr := acquirePostCommitBatchLock(ctx, repo.Dir)
		acquired <- result{unlock: unlock, err: lockErr}
	}()
	<-started

	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	var early result
	returnedEarly := false
	select {
	case early = <-acquired:
		returnedEarly = true
	case <-timer.C:
	}
	if early.unlock != nil {
		defer func() { _ = early.unlock() }()
	}
	require.False(t, returnedEarly)
	require.NoError(t, firstUnlock())

	second := <-acquired
	require.NoError(t, second.err)
	require.NotNil(t, second.unlock)
	require.NoError(t, second.unlock())
}

func TestAcquirePostCommitBatchLockTimesOutOnStuckHolder(t *testing.T) {
	repo := newTestGitRepo(t)
	unlock, err := acquirePostCommitBatchLock(t.Context(), repo.Dir)
	require.NoError(t, err)
	defer func() { _ = unlock() }()
	old := postCommitBatchLockTimeout
	postCommitBatchLockTimeout = 50 * time.Millisecond
	t.Cleanup(func() { postCommitBatchLockTimeout = old })

	// A stuck or suspended holder must not block later hooks (and with them
	// the user's commits) indefinitely: the waiter bails, its checkpoint
	// stays unchanged, and the next hook run retries the same range.
	start := time.Now()
	second, err := acquirePostCommitBatchLock(t.Context(), repo.Dir)
	if second != nil {
		defer func() { _ = second() }()
	}
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestPostCommitBatchStateRoundTrip(t *testing.T) {
	repo := newTestGitRepo(t)
	path, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	want := postCommitBatchState{
		Version: postCommitBatchStateVersion,
		Branches: map[string]postCommitBatchEntry{
			"feature/example": {Checkpoint: strings.Repeat("a", 40)},
		},
	}

	require.NoError(t, savePostCommitBatchState(path, want))
	got, exists, err := loadPostCommitBatchState(path)

	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, want, got)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestPostCommitBatchStateReadsLegacyBareCheckpoints(t *testing.T) {
	repo := newTestGitRepo(t)
	path, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	sha := strings.Repeat("a", 40)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, fmt.Appendf(nil,
		`{"version":1,"branches":{"feature":%q}}`, sha), 0o600))

	state, exists, err := loadPostCommitBatchState(path)

	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, postCommitBatchEntry{Checkpoint: sha}, state.Branches["feature"])
}

func TestPostCommitBatchStateIsSharedAcrossWorktrees(t *testing.T) {
	mainRepo := newTestGitRepo(t)
	mainRepo.CommitFile("base.txt", "base", "base")
	worktreeDir := t.TempDir()
	mainRepo.Run("worktree", "add", worktreeDir, "-b", "worktree-branch")

	mainPath, err := postCommitBatchStatePath(mainRepo.Dir)
	require.NoError(t, err)
	worktreePath, err := postCommitBatchStatePath(worktreeDir)
	require.NoError(t, err)
	assert.Equal(t, mainPath, worktreePath)

	require.NoError(t, savePostCommitBatchState(mainPath, postCommitBatchState{
		Version: postCommitBatchStateVersion,
		Branches: map[string]postCommitBatchEntry{
			"main": {Checkpoint: mainRepo.HeadSHA()},
		},
	}))
	state, exists, err := loadPostCommitBatchState(worktreePath)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, mainRepo.HeadSHA(), state.Branches["main"].Checkpoint)
}

func TestPostCommitBatchStateKeepsBranchesSeparate(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature-one")
	repo.CommitFile("one.txt", "one", "one")
	one := planPostCommitBatch(t.Context(), repo.Dir, 5)
	assert.Equal(t, base, one.Checkpoint)

	repo.Run("checkout", "-b", "feature-two", base)
	repo.CommitFile("two.txt", "two", "two")
	two := planPostCommitBatch(t.Context(), repo.Dir, 5)
	assert.Equal(t, base, two.Checkpoint)

	state, exists, err := loadPostCommitBatchState(two.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, base, state.Branches["feature-one"].Checkpoint)
	assert.Equal(t, base, state.Branches["feature-two"].Checkpoint)
}

func TestPlanPostCommitBatchMigratesCheckpointAfterBranchRename(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("old-name")
	repo.CommitFile("one.txt", "one", "one")
	first := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, first.Checkpoint)

	repo.CommitFile("two.txt", "two", "two")
	second := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, 2, second.Pending)
	repo.Run("branch", "-m", "new-name")
	head := repo.CommitFile("three.txt", "three", "three")

	renamed := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert.False(t, renamed.Ready)
	assert.Equal(t, 3, renamed.Pending)
	assert.Equal(t, base, renamed.Checkpoint)
	assert.Equal(t, base+".."+head, renamed.AccumulatedRef())
	state, exists, err := loadPostCommitBatchState(renamed.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.NotContains(t, state.Branches, "old-name")
	assert.Equal(t, base, state.Branches["new-name"].Checkpoint)
}

func TestPlanPostCommitBatchMigratesRenamedBranchWithoutReflog(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("old-name")
	repo.CommitFile("one.txt", "one", "one")
	first := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, first.Checkpoint)
	repo.Run("branch", "-m", "new-name")
	gitDir := repo.Run("rev-parse", "--git-dir")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repo.Dir, gitDir)
	}
	// Expired or pruned reflogs must not cost the renamed branch its pending
	// range: migration relies only on the orphaned entry's recorded range
	// lying on this branch's first-parent chain.
	require.NoError(t, os.RemoveAll(filepath.Join(gitDir, "logs")))
	head := repo.CommitFile("two.txt", "two", "two")

	renamed := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert.Equal(t, base, renamed.Checkpoint)
	assert.Equal(t, 2, renamed.Pending)
	assert.Equal(t, base+".."+head, renamed.AccumulatedRef())
	state, exists, err := loadPostCommitBatchState(renamed.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.NotContains(t, state.Branches, "old-name")
	assert.Equal(t, base, state.Branches["new-name"].Checkpoint)
}

func TestPlanPostCommitBatchMigratesAcrossConsecutiveRenames(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("old")
	repo.CommitFile("one.txt", "one", "one")
	first := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, first.Checkpoint)
	repo.Run("branch", "-m", "old", "middle")
	repo.Run("branch", "-m", "middle", "new")
	head := repo.CommitFile("two.txt", "two", "two")

	renamed := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert.Equal(t, base, renamed.Checkpoint)
	assert.Equal(t, 2, renamed.Pending)
	assert.Equal(t, base+".."+head, renamed.AccumulatedRef())
	state, exists, err := loadPostCommitBatchState(renamed.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.NotContains(t, state.Branches, "old")
	assert.NotContains(t, state.Branches, "middle")
	assert.Equal(t, base, state.Branches["new"].Checkpoint)
}

func TestPlanPostCommitBatchRecreatedSourceKeepsPendingRange(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("old")
	repo.CommitFile("o1.txt", "o1", "o1")
	first := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, first.Checkpoint)
	repo.Run("branch", "-m", "old", "new")

	// "old" is recreated and runs its hook before the renamed branch's first
	// run. Its recorded tip escaped to "new", so the pending range must be
	// handed off there while the recreated branch tracks only its own commit.
	repo.Run("checkout", "-b", "old", base)
	repo.CommitFile("r1.txt", "r1", "r1")
	recreated := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, recreated.Checkpoint)
	require.Equal(t, 1, recreated.Pending)

	repo.Run("checkout", "new")
	head := repo.CommitFile("n1.txt", "n1", "n1")
	renamed := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert := assert.New(t)
	assert.Equal(base, renamed.Checkpoint,
		"the renamed branch must inherit the handed-off pre-rename range")
	assert.Equal(2, renamed.Pending)
	assert.Equal(base+".."+head, renamed.AccumulatedRef())
	state, exists, err := loadPostCommitBatchState(renamed.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(base, state.Branches["old"].Checkpoint,
		"the recreated branch keeps tracking its own commits")
	assert.Equal(base, state.Branches["new"].Checkpoint)
}

func TestPlanPostCommitBatchParentDoesNotStealRenamedBranchRange(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("base.txt", "base", "base")
	mainBranch := repo.Run("branch", "--show-current")
	baseTwo := repo.CommitFile("b2.txt", "b2", "b2")
	mainPlan := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.True(t, mainPlan.Track)
	repo.CheckoutNewBranch("work")
	repo.CommitFile("x1.txt", "x1", "x1")
	workPlan := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, baseTwo, workPlan.Checkpoint)
	repo.Run("branch", "-m", "work", "renamed")

	// The orphaned entry's checkpoint sits on the parent's chain too, but its
	// recorded tip does not: the pending commit belongs to the renamed
	// branch, and the parent adopting (and deleting) the entry would strand it.
	repo.Run("checkout", mainBranch)
	repo.CommitFile("m1.txt", "m1", "m1")
	parent := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, mainPlan.Checkpoint, parent.Checkpoint)
	state, exists, err := loadPostCommitBatchState(parent.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, baseTwo, state.Branches["work"].Checkpoint,
		"the parent must leave the renamed branch's pending range alone")

	repo.Run("checkout", "renamed")
	head := repo.CommitFile("y1.txt", "y1", "y1")
	renamed := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert.Equal(t, baseTwo, renamed.Checkpoint)
	assert.Equal(t, 2, renamed.Pending)
	assert.Equal(t, baseTwo+".."+head, renamed.AccumulatedRef())
}

func TestPlanPostCommitBatchForkAtEarlierTipDoesNotEatLaterCommits(
	t *testing.T,
) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("work")
	wOne := repo.CommitFile("w1.txt", "w1", "w1")
	plan := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, plan.Checkpoint)
	repo.CommitFile("w2.txt", "w2", "w2")
	plan = planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, 2, plan.Pending)
	wThree := repo.CommitFile("w3.txt", "w3", "w3")
	plan = planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, 3, plan.Pending)

	// A branch forked at an earlier pending commit sees the orphan's
	// checkpoint on its own chain after the rename, but the range's recorded
	// end is not. The fork must neither adopt nor delete the entry — doing
	// so would silently drop w2 and w3 from batch state.
	repo.Run("branch", "fork", wOne)
	repo.Run("branch", "-m", "work", "renamed")
	repo.Run("checkout", "fork")
	repo.CommitFile("f1.txt", "f1", "f1")
	fork := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert := assert.New(t)
	assert.Equal(wOne, fork.Checkpoint)
	assert.Equal(1, fork.Pending)
	state, exists, err := loadPostCommitBatchState(fork.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(base, state.Branches["work"].Checkpoint)
	assert.Equal(wThree, state.Branches["work"].Tip,
		"the recorded tip must track every pending commit")

	repo.Run("checkout", "renamed")
	head := repo.CommitFile("r1.txt", "r1", "r1")
	renamed := planPostCommitBatch(t.Context(), repo.Dir, 5)
	assert.Equal(base, renamed.Checkpoint)
	assert.Equal(4, renamed.Pending)
	assert.Equal(base+".."+head, renamed.AccumulatedRef())
}

func TestPlanPostCommitBatchDoesNotAdoptRecreatedRenameSource(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("old")
	firstHead := repo.CommitFile("one.txt", "one", "one")
	first := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, first.Checkpoint)
	repo.Run("branch", "-m", "old", "new")
	renamed := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, renamed.Checkpoint)

	repo.Run("checkout", "-b", "old", firstHead)
	repo.CommitFile("recreated.txt", "recreated", "recreated")
	recreated := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, firstHead, recreated.Checkpoint)
	repo.Run("checkout", "new")
	repo.Run("branch", "-D", "old")
	head := repo.CommitFile("two.txt", "two", "two")

	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert := assert.New(t)
	assert.Equal(base, decision.Checkpoint)
	assert.Equal(2, decision.Pending)
	assert.Equal(base+".."+head, decision.AccumulatedRef())
}

func TestPlanPostCommitBatchKeepsBranchAndHeadConsistentDuringCheckout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX git wrapper")
	}
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	featureHead := repo.CommitFile("feature.txt", "feature", "feature")
	initial := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, initial.Checkpoint)
	repo.Run("checkout", "-b", "other", base)
	repo.CommitFile("other.txt", "other", "other")
	repo.Run("checkout", "feature")

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(`#!/bin/sh
if [ "$1" = "symbolic-ref" ] && [ "$2" = "HEAD" ]; then
  "$REAL_GIT" "$@"
  "$REAL_GIT" checkout other >/dev/null 2>&1
  exit
fi
exec "$REAL_GIT" "$@"
`), 0o755))
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert := assert.New(t)
	assert.Equal("feature", decision.Branch)
	assert.Equal(featureHead, decision.Head)
	assert.Equal(base+".."+featureHead, decision.AccumulatedRef())
}

func TestPlanPostCommitBatchRecoversWhenBranchRecreationIsInconclusive(
	t *testing.T,
) {
	for _, tc := range []struct {
		name      string
		divergent bool
	}{
		{name: "related history"},
		{name: "divergent history", divergent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newTestGitRepo(t)
			repo.CommitFile("base.txt", "base", "base")
			mainBranch := repo.Run("branch", "--show-current")
			repo.CheckoutNewBranch("feature")
			oldOne := repo.CommitFile("old-one.txt", "one", "old one")
			oldTwo := repo.CommitFile("old-two.txt", "two", "old two")
			repo.Run("update-ref", "--create-reflog", "-m", "rewind",
				"refs/heads/feature", oldOne, oldTwo)
			repo.Run("update-ref", "-m", "commit: old two",
				"refs/heads/feature", oldTwo, oldOne)
			initial := planPostCommitBatch(t.Context(), repo.Dir, 5)
			require.Equal(t, oldOne, initial.Checkpoint)

			expectedCheckpoint := oldOne
			if tc.divergent {
				repo.Run("checkout", "--orphan", "replacement")
				newRoot := repo.CommitFile("new-root.txt", "root", "new root")
				expectedCheckpoint = newRoot + "^"
				repo.Run("branch", "-D", "feature")
				repo.Run("branch", "-m", "feature")
			} else {
				repo.Run("checkout", mainBranch)
				repo.Run("branch", "-D", "feature")
				repo.Run("checkout", "-b", "feature", oldTwo)
			}
			head := repo.CommitFile("new.txt", "new", "new")

			decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

			assert := assert.New(t)
			assert.Equal(expectedCheckpoint, decision.Checkpoint)
			assert.Equal(2, decision.Pending)
			assert.Equal(expectedCheckpoint+".."+head, decision.AccumulatedRef())
		})
	}
}

func TestPlanPostCommitBatchPreservesCheckpointAfterReflogPruning(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	one := repo.CommitFile("one.txt", "one", "one")
	repo.Run("update-ref", "--create-reflog", "-m", "rewind",
		"refs/heads/feature", base, one)
	repo.Run("update-ref", "-m", "restore",
		"refs/heads/feature", one, base)
	initial := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, initial.Checkpoint)

	gitDir := repo.Run("rev-parse", "--git-dir")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repo.Dir, gitDir)
	}
	require.NoError(t, os.Remove(filepath.Join(
		gitDir, "logs", "refs", "heads", "feature",
	)))
	head := repo.CommitFile("two.txt", "two", "two")
	repo.Run("update-ref", "--create-reflog", "-m", "rewind",
		"refs/heads/feature", one, head)
	repo.Run("update-ref", "-m", "restore",
		"refs/heads/feature", head, one)
	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert.Equal(t, base, decision.Checkpoint)
	assert.Equal(t, 2, decision.Pending)
	assert.Equal(t, base+".."+head, decision.AccumulatedRef())
}

func TestPlanPostCommitBatchReconcilesRenameOntoStaleBranchName(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("work")
	wOne := repo.CommitFile("w1.txt", "w1", "w1")
	head := repo.CommitFile("w2.txt", "w2", "w2")
	repo.Run("checkout", "-b", "side", wOne)
	stale := repo.CommitFile("s1.txt", "s1", "s1")
	repo.Run("checkout", "work")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	// "feature" once existed, accumulated state, and was deleted; its stale
	// off-chain checkpoint would make merge-base recovery drain from w1,
	// permanently skipping base..w1 pending under the renamed branch.
	require.NoError(t, savePostCommitBatchState(statePath, postCommitBatchState{
		Version: postCommitBatchStateVersion,
		Branches: map[string]postCommitBatchEntry{
			"work": {Checkpoint: base}, "feature": {Checkpoint: stale},
		},
	}))
	repo.Run("branch", "-m", "work", "feature")

	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert := assert.New(t)
	assert.Equal(base, decision.Checkpoint,
		"rename evidence must override the stale reused-name checkpoint")
	assert.Equal(2, decision.Pending)
	assert.Equal(base+".."+head, decision.AccumulatedRef())
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.NotContains(state.Branches, "work")
	assert.Equal(base, state.Branches["feature"].Checkpoint)
}

func TestPlanPostCommitBatchReconcilesOnChainCheckpointAfterRename(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("work")
	repo.CommitFile("w1.txt", "w1", "w1")
	stale := repo.CommitFile("w2.txt", "w2", "w2")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	require.NoError(t, savePostCommitBatchState(statePath, postCommitBatchState{
		Version: postCommitBatchStateVersion,
		Branches: map[string]postCommitBatchEntry{
			"work": {Checkpoint: base}, "feature": {Checkpoint: stale},
		},
	}))
	repo.Run("branch", "-m", "work", "feature")
	head := repo.CommitFile("w3.txt", "w3", "w3")

	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert := assert.New(t)
	assert.Equal(base, decision.Checkpoint)
	assert.Equal(3, decision.Pending)
	assert.Equal(base+".."+head, decision.AccumulatedRef())
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.NotContains(state.Branches, "work")
	assert.Equal(base, state.Branches["feature"].Checkpoint)
}

func TestPlanPostCommitBatchAdoptsOrphanedOnChainCheckpoint(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	repo.CommitFile("one.txt", "one", "one")
	head := repo.CommitFile("two.txt", "two", "two")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	// A gone branch's entry lingers with a checkpoint on feature's
	// first-parent chain: commits that were counted as pending but never
	// reviewed. Adopting it costs at most a redundant review; discarding it
	// could skip those commits.
	require.NoError(t, savePostCommitBatchState(statePath, postCommitBatchState{
		Version: postCommitBatchStateVersion,
		Branches: map[string]postCommitBatchEntry{
			"deleted-branch": {Checkpoint: base},
		},
	}))

	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert := assert.New(t)
	assert.Equal(base, decision.Checkpoint,
		"orphaned on-chain checkpoints must be adopted, not discarded")
	assert.Equal(2, decision.Pending)
	assert.Equal(base+".."+head, decision.AccumulatedRef())
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.NotContains(state.Branches, "deleted-branch")
	assert.Equal(base, state.Branches["feature"].Checkpoint)
}

func TestPlanPostCommitBatchKeepsOffChainOrphanForItsOwnHistory(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("side")
	sideTip := repo.CommitFile("side.txt", "side", "side")
	repo.Run("checkout", "-b", "feature", base)
	repo.CommitFile("one.txt", "one", "one")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	// "gone" tracked side history; its checkpoint is not on feature's
	// first-parent chain, so feature must neither adopt nor delete it — a
	// branch on that history may still cover it later.
	require.NoError(t, savePostCommitBatchState(statePath, postCommitBatchState{
		Version: postCommitBatchStateVersion,
		Branches: map[string]postCommitBatchEntry{
			"gone": {Checkpoint: sideTip},
		},
	}))

	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert.Equal(t, base, decision.Checkpoint,
		"feature must seed at HEAD^1 and leave the off-chain orphan alone")
	assert.Equal(t, 1, decision.Pending)
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, sideTip, state.Branches["gone"].Checkpoint)
	assert.Equal(t, base, state.Branches["feature"].Checkpoint)
}

func TestPlanPostCommitBatchTracksWindows(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	repo.CommitFile("one.txt", "one", "one")

	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)
	assert.False(t, decision.Ready)
	assert.Equal(t, 1, decision.Pending)
	assert.Equal(t, base, decision.Checkpoint)

	for i := 2; i <= 4; i++ {
		repo.CommitFile(
			filepath.Join("commits", string(rune('0'+i))+".txt"),
			"change", "change",
		)
		decision = planPostCommitBatch(t.Context(), repo.Dir, 5)
		assert.False(t, decision.Ready)
		assert.Equal(t, i, decision.Pending)
	}

	headFive := repo.CommitFile("five.txt", "five", "five")
	decision = planPostCommitBatch(t.Context(), repo.Dir, 5)
	assert.True(t, decision.Ready)
	assert.Equal(t, 5, decision.Pending)
	assert.Equal(t, base+".."+headFive, decision.AccumulatedRef())
	require.NoError(t, advancePostCommitBatch(t.Context(), repo.Dir, decision))

	repo.CommitFile("six.txt", "six", "six")
	next := planPostCommitBatch(t.Context(), repo.Dir, 5)
	assert.False(t, next.Ready)
	assert.Equal(t, 1, next.Pending)
	assert.Equal(t, headFive, next.Checkpoint)
}

func TestPlanPostCommitBatchDisabledPreservesImmediateReview(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("base.txt", "base", "base")

	decision := planPostCommitBatch(t.Context(), repo.Dir, 1)

	assert.False(t, decision.Enabled)
	assert.False(t, decision.Track)
	assert.True(t, decision.Ready)
	assert.Equal(t, "HEAD", decision.AccumulatedRef())
	path, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestPlanPostCommitBatchDisabledClearsExistingStateAfterDrain(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	repo.CommitFile("one.txt", "one", "one")
	first := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.True(t, first.Track)
	head := repo.CommitFile("two.txt", "two", "two")

	disabled := planPostCommitBatch(t.Context(), repo.Dir, 1)
	require.True(t, disabled.Ready)
	assert.True(t, disabled.Enabled)
	assert.True(t, disabled.Track)
	assert.Equal(t, first.Checkpoint+".."+head, disabled.AccumulatedRef())
	require.NoError(t, advancePostCommitBatch(t.Context(), repo.Dir, disabled))

	state, exists, err := loadPostCommitBatchState(disabled.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.NotContains(t, state.Branches, "feature")
}

func TestPlanPostCommitBatchDisabledDrainsOffChainCheckpoint(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	head := repo.CommitFile("feature.txt", "feature", "feature")
	first := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.True(t, first.Track)
	repo.Run("checkout", "-b", "side", base)
	side := repo.CommitFile("side.txt", "side", "side")
	repo.Run("checkout", "feature")
	state, exists, err := loadPostCommitBatchState(first.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	state.Branches["feature"] = postCommitBatchEntry{Checkpoint: side}
	require.NoError(t, savePostCommitBatchState(first.statePath, state))

	disabled := planPostCommitBatch(t.Context(), repo.Dir, 1)

	assert.True(t, disabled.Ready)
	assert.True(t, disabled.Enabled)
	assert.Equal(t, base+".."+head, disabled.AccumulatedRef())
	require.NoError(t, advancePostCommitBatch(t.Context(), repo.Dir, disabled))
	state, exists, err = loadPostCommitBatchState(disabled.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.NotContains(t, state.Branches, "feature")
}

func TestPlanPostCommitBatchDisabledPreservesUnreadableCheckpoint(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	repo.CommitFile("feature.txt", "feature", "feature")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	badCheckpoint := strings.Repeat("f", 40)
	require.NoError(t, savePostCommitBatchState(statePath, postCommitBatchState{
		Version: postCommitBatchStateVersion,
		Branches: map[string]postCommitBatchEntry{
			"feature": {Checkpoint: badCheckpoint},
		},
	}))

	disabled := planPostCommitBatch(t.Context(), repo.Dir, 1)

	assert.True(t, disabled.Ready)
	assert.False(t, disabled.Enabled)
	assert.True(t, disabled.PreserveCheckpoint)
	require.NoError(t, advancePostCommitBatch(t.Context(), repo.Dir, disabled))
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, badCheckpoint, state.Branches["feature"].Checkpoint)
}

func TestPlanPostCommitBatchRootCommitFailsOpen(t *testing.T) {
	repo := newTestGitRepo(t)
	head := repo.CommitFile("root.txt", "root", "root")

	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert.True(t, decision.Enabled)
	assert.True(t, decision.Ready)
	assert.NotEmpty(t, decision.Reason)
	require.NoError(t, advancePostCommitBatch(t.Context(), repo.Dir, decision))
	state, exists, err := loadPostCommitBatchState(decision.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	branch := repo.Run("branch", "--show-current")
	assert.Equal(t, head, state.Branches[branch].Checkpoint)
}

func TestPlanPostCommitBatchCorruptStateFailsOpen(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("base.txt", "base", "base")
	path, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("not json\n"), 0o600))

	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert.True(t, decision.Ready)
	assert.Contains(t, decision.Reason, "state")
	require.NoError(t, advancePostCommitBatch(t.Context(), repo.Dir, decision))
	state, exists, err := loadPostCommitBatchState(path)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, decision.Head, state.Branches[decision.Branch].Checkpoint)
}

func TestPlanPostCommitBatchRecoversOffChainCheckpoint(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	head := repo.CommitFile("feature.txt", "feature", "feature")
	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	repo.Run("checkout", "-b", "side", base)
	side := repo.CommitFile("side.txt", "side", "side")
	repo.Run("checkout", "feature")
	state, exists, err := loadPostCommitBatchState(decision.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	state.Branches["feature"] = postCommitBatchEntry{Checkpoint: side}
	require.NoError(t, savePostCommitBatchState(decision.statePath, state))

	rewritten := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert.True(t, rewritten.Ready)
	assert.False(t, rewritten.PreserveCheckpoint)
	assert.Equal(t, base+".."+head, rewritten.AccumulatedRef())
	assert.Contains(t, rewritten.Reason, "first-parent")
}

func TestPlanPostCommitBatchRecoversUnrelatedHistoryFromEmptyTree(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	repo.CommitFile("old.txt", "old", "old")
	initial := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, initial.Checkpoint)

	repo.Run("checkout", "--orphan", "rewritten")
	root := repo.CommitFile("new-root.txt", "root", "new root")
	head := repo.CommitFile("new-head.txt", "head", "new head")
	repo.Run("branch", "-D", "feature")
	repo.Run("branch", "-m", "feature")

	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert := assert.New(t)
	assert.True(decision.Ready)
	assert.False(decision.PreserveCheckpoint)
	assert.Equal(2, decision.Pending)
	assert.Equal(root+"^", decision.Checkpoint)
	assert.Equal(root+"^.."+head, decision.AccumulatedRef())
	require.NoError(t, advancePostCommitBatch(t.Context(), repo.Dir, decision))
	state, exists, err := loadPostCommitBatchState(decision.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(head, state.Branches["feature"].Checkpoint)
}

func TestPlanStoredPostCommitBatchCorruptStateFailsOpen(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	head := repo.CommitFile("feature.txt", "feature", "feature")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	require.NoError(t, os.WriteFile(statePath, []byte("not json\n"), 0o600))

	decision := planStoredPostCommitBatch(
		t.Context(), repo.Dir, 5, "feature", "",
	)

	assert := assert.New(t)
	assert.True(decision.Ready)
	assert.Equal("feature", decision.Branch)
	assert.Equal(head, decision.Head)
	assert.Equal(head, decision.AccumulatedRef())
	assert.Contains(decision.Reason, "state")
	require.NoError(t, advancePostCommitBatch(t.Context(), repo.Dir, decision))
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(head, state.Branches["feature"].Checkpoint)
}

func TestPlanStoredPostCommitBatchRecoversOffChainCheckpoint(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	head := repo.CommitFile("feature.txt", "feature", "feature")
	repo.Run("checkout", "-b", "side", base)
	side := repo.CommitFile("side.txt", "side", "side")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	require.NoError(t, savePostCommitBatchState(statePath, postCommitBatchState{
		Version: postCommitBatchStateVersion,
		Branches: map[string]postCommitBatchEntry{
			"feature": {Checkpoint: side}, "side": {Checkpoint: base},
		},
	}))

	decision := planStoredPostCommitBatch(
		t.Context(), repo.Dir, 5, "feature", "",
	)

	assert := assert.New(t)
	assert.True(decision.Ready)
	assert.False(decision.PreserveCheckpoint)
	assert.Equal(base+".."+head, decision.AccumulatedRef())
	assert.Contains(decision.Reason, "first-parent")
	require.NoError(t, advancePostCommitBatch(t.Context(), repo.Dir, decision))
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(head, state.Branches["feature"].Checkpoint)
	assert.Equal(base, state.Branches["side"].Checkpoint)
}

func TestPlanStoredPostCommitBatchInvalidCheckpointFailsOpen(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	head := repo.CommitFile("feature.txt", "feature", "feature")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	require.NoError(t, savePostCommitBatchState(statePath, postCommitBatchState{
		Version: postCommitBatchStateVersion,
		Branches: map[string]postCommitBatchEntry{
			"feature": {Checkpoint: strings.Repeat("f", 40)},
		},
	}))

	decision := planStoredPostCommitBatch(
		t.Context(), repo.Dir, 5, "feature", "",
	)

	assert := assert.New(t)
	assert.True(decision.Ready)
	assert.True(decision.PreserveCheckpoint)
	assert.Equal(head, decision.AccumulatedRef())
	assert.Contains(decision.Reason, "count")
	require.NoError(t, advancePostCommitBatch(t.Context(), repo.Dir, decision))
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(strings.Repeat("f", 40), state.Branches["feature"].Checkpoint)
}

func TestAdvancePostCommitBatchRepairPreservesOtherBranches(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature-one")
	repo.CommitFile("one.txt", "one", "one")
	one := planPostCommitBatch(t.Context(), repo.Dir, 5)

	repo.Run("checkout", "-b", "feature-two", base)
	repo.CommitFile("two.txt", "two", "two")
	two := planPostCommitBatch(t.Context(), repo.Dir, 5)

	repo.Run("checkout", "-b", "side", base)
	side := repo.CommitFile("side.txt", "side", "side")
	repo.Run("checkout", "feature-one")
	state, exists, err := loadPostCommitBatchState(one.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	state.Branches["feature-one"] = postCommitBatchEntry{Checkpoint: side}
	require.NoError(t, savePostCommitBatchState(one.statePath, state))

	repair := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.NotEmpty(t, repair.Reason)
	require.NoError(t, advancePostCommitBatch(t.Context(), repo.Dir, repair))

	state, exists, err = loadPostCommitBatchState(repair.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, repair.Head, state.Branches["feature-one"].Checkpoint)
	assert.Equal(t, two.Checkpoint, state.Branches["feature-two"].Checkpoint)
}
