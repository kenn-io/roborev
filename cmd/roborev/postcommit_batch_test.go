package main

import (
	"context"
	"os"
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

func TestPostCommitBatchStateRoundTrip(t *testing.T) {
	repo := newTestGitRepo(t)
	path, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	want := postCommitBatchState{
		Version:  postCommitBatchStateVersion,
		Branches: map[string]string{"feature/example": strings.Repeat("a", 40)},
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
		Version:  postCommitBatchStateVersion,
		Branches: map[string]string{"main": mainRepo.HeadSHA()},
	}))
	state, exists, err := loadPostCommitBatchState(worktreePath)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, mainRepo.HeadSHA(), state.Branches["main"])
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
	assert.Equal(t, base, state.Branches["feature-one"])
	assert.Equal(t, base, state.Branches["feature-two"])
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
	assert.Equal(t, base, state.Branches["new-name"])
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
	assert.Equal(t, base, state.Branches["new"])
}

func TestPlanPostCommitBatchMigrationSkipsRecreatedSourceBranch(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("old")
	oOne := repo.CommitFile("o1.txt", "o1", "o1")
	first := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, first.Checkpoint)
	repo.Run("branch", "-m", "old", "new")

	// "old" is recreated and accumulates its own batch before the renamed
	// branch's first hook run; migration must not steal its live entry.
	repo.Run("checkout", "-b", "old", base)
	repo.CommitFile("r1.txt", "r1", "r1")
	recreated := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.Equal(t, base, recreated.Checkpoint)
	require.Equal(t, 1, recreated.Pending)

	repo.Run("checkout", "new")
	head := repo.CommitFile("n1.txt", "n1", "n1")
	renamed := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert := assert.New(t)
	assert.Equal(oOne, renamed.Checkpoint,
		"the renamed branch must seed fresh instead of stealing the entry")
	assert.Equal(1, renamed.Pending)
	assert.Equal(oOne+".."+head, renamed.AccumulatedRef())
	state, exists, err := loadPostCommitBatchState(renamed.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(base, state.Branches["old"],
		"the recreated branch's live checkpoint must be preserved")
	assert.Equal(oOne, state.Branches["new"])
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
		Version:  postCommitBatchStateVersion,
		Branches: map[string]string{"work": base, "feature": stale},
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
	assert.Equal(base, state.Branches["feature"])
}

func TestPlanPostCommitBatchIgnoresStaleCheckpointWithoutRename(t *testing.T) {
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	one := repo.CommitFile("one.txt", "one", "one")
	repo.CommitFile("two.txt", "two", "two")
	statePath, err := postCommitBatchStatePath(repo.Dir)
	require.NoError(t, err)
	// A deleted branch's entry lingers with a checkpoint on feature's
	// first-parent chain. Without reflog rename evidence, a new branch must
	// not adopt it and review a historical range it never accumulated.
	require.NoError(t, savePostCommitBatchState(statePath, postCommitBatchState{
		Version:  postCommitBatchStateVersion,
		Branches: map[string]string{"deleted-branch": base},
	}))

	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert.Equal(t, one, decision.Checkpoint,
		"stale checkpoints without rename evidence must seed at HEAD^1")
	assert.Equal(t, 1, decision.Pending)
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, base, state.Branches["deleted-branch"])
	assert.Equal(t, one, state.Branches["feature"])
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
	require.NoError(t, advancePostCommitBatch(decision))

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
	require.NoError(t, advancePostCommitBatch(disabled))

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
	state.Branches["feature"] = side
	require.NoError(t, savePostCommitBatchState(first.statePath, state))

	disabled := planPostCommitBatch(t.Context(), repo.Dir, 1)

	assert.True(t, disabled.Ready)
	assert.True(t, disabled.Enabled)
	assert.Equal(t, base+".."+head, disabled.AccumulatedRef())
	require.NoError(t, advancePostCommitBatch(disabled))
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
		Version:  postCommitBatchStateVersion,
		Branches: map[string]string{"feature": badCheckpoint},
	}))

	disabled := planPostCommitBatch(t.Context(), repo.Dir, 1)

	assert.True(t, disabled.Ready)
	assert.False(t, disabled.Enabled)
	assert.True(t, disabled.PreserveCheckpoint)
	require.NoError(t, advancePostCommitBatch(disabled))
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, badCheckpoint, state.Branches["feature"])
}

func TestPlanPostCommitBatchRootCommitFailsOpen(t *testing.T) {
	repo := newTestGitRepo(t)
	head := repo.CommitFile("root.txt", "root", "root")

	decision := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert.True(t, decision.Enabled)
	assert.True(t, decision.Ready)
	assert.NotEmpty(t, decision.Reason)
	require.NoError(t, advancePostCommitBatch(decision))
	state, exists, err := loadPostCommitBatchState(decision.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	branch := repo.Run("branch", "--show-current")
	assert.Equal(t, head, state.Branches[branch])
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
	require.NoError(t, advancePostCommitBatch(decision))
	state, exists, err := loadPostCommitBatchState(path)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, decision.Head, state.Branches[decision.Branch])
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
	state.Branches["feature"] = side
	require.NoError(t, savePostCommitBatchState(decision.statePath, state))

	rewritten := planPostCommitBatch(t.Context(), repo.Dir, 5)

	assert.True(t, rewritten.Ready)
	assert.False(t, rewritten.PreserveCheckpoint)
	assert.Equal(t, base+".."+head, rewritten.AccumulatedRef())
	assert.Contains(t, rewritten.Reason, "first-parent")
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
	require.NoError(t, advancePostCommitBatch(decision))
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(head, state.Branches["feature"])
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
		Version:  postCommitBatchStateVersion,
		Branches: map[string]string{"feature": side, "side": base},
	}))

	decision := planStoredPostCommitBatch(
		t.Context(), repo.Dir, 5, "feature", "",
	)

	assert := assert.New(t)
	assert.True(decision.Ready)
	assert.False(decision.PreserveCheckpoint)
	assert.Equal(base+".."+head, decision.AccumulatedRef())
	assert.Contains(decision.Reason, "first-parent")
	require.NoError(t, advancePostCommitBatch(decision))
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(head, state.Branches["feature"])
	assert.Equal(base, state.Branches["side"])
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
		Branches: map[string]string{
			"feature": strings.Repeat("f", 40),
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
	require.NoError(t, advancePostCommitBatch(decision))
	state, exists, err := loadPostCommitBatchState(statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(strings.Repeat("f", 40), state.Branches["feature"])
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
	state.Branches["feature-one"] = side
	require.NoError(t, savePostCommitBatchState(one.statePath, state))

	repair := planPostCommitBatch(t.Context(), repo.Dir, 5)
	require.NotEmpty(t, repair.Reason)
	require.NoError(t, advancePostCommitBatch(repair))

	state, exists, err = loadPostCommitBatchState(repair.statePath)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, repair.Head, state.Branches["feature-one"])
	assert.Equal(t, two.Checkpoint, state.Branches["feature-two"])
}
