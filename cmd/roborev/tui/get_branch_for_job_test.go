package tui

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// TestGetBranchForJobDetachedHead covers the queue view's branch fallback
// (#499): a review job with no stored branch, enqueued from a commit made on
// top of a detached HEAD (e.g. mid git-bisect), has no branch reachable via
// git name-rev either. Instead of caching and returning "", getBranchForJob
// should surface a "(detached @ <sha>)" placeholder.
func TestGetBranchForJobDetachedHead(t *testing.T) {
	t.Parallel()
	repo := testutil.InitTestRepo(t)
	repo.CommitFile("a.txt", "one", "first")
	// Detach HEAD and commit again, mirroring a commit made mid git-bisect:
	// the resulting SHA is reachable from HEAD but from no local branch.
	repo.CheckoutDetached()
	sha := repo.CommitFile("a.txt", "two", "second, on detached HEAD")

	commitID := int64(1)
	job := storage.ReviewJob{
		ID:       1,
		JobType:  storage.JobTypeReview,
		GitRef:   sha,
		RepoPath: repo.Path(),
		CommitID: &commitID,
	}

	m := newModel(localhostEndpoint, withExternalIODisabled())
	got := m.getBranchForJobCtx(t.Context(), job)

	assert.Equal(t, "(detached @ "+sha[:7]+")", got)

	// Result is cached rather than re-derived on the next call.
	assert.Equal(t, got, m.branchNames[job.ID])

	// The canonical queue row for a detached single-commit panel review is
	// its synthesis job; it gets the same placeholder.
	synthesis := storage.ReviewJob{
		ID:       2,
		JobType:  storage.JobTypeSynthesis,
		GitRef:   sha,
		RepoPath: repo.Path(),
		CommitID: &commitID,
	}
	assert.Equal(t, "(detached @ "+sha[:7]+")", m.getBranchForJobCtx(t.Context(), synthesis))
}

// TestBackfillBranchValue covers the once-per-session branch backfill
// decision (#499 follow-up): detached single-commit reviews are left
// unbackfilled so their empty stored branch keeps rendering the detached
// placeholder, while task/remote/reachable jobs persist as before.
func TestBackfillBranchValue(t *testing.T) {
	t.Parallel()
	repo := testutil.InitTestRepo(t)
	firstSHA := repo.CommitFile("a.txt", "one", "first")
	repo.CheckoutDetached()
	detachedSHA := repo.CommitFile("a.txt", "two", "second, on detached HEAD")
	// Branch from the first commit so detachedSHA stays unreachable.
	repo.CheckoutNewBranch("feature-x", firstSHA)
	reachableSHA := repo.CommitFile("b.txt", "three", "third, on a branch")

	commitID := int64(1)

	t.Run("detached review left unbackfilled", func(t *testing.T) {
		job := storage.ReviewJob{JobType: storage.JobTypeReview, GitRef: detachedSHA, RepoPath: repo.Path(), CommitID: &commitID}
		_, ok := backfillBranchValue(job, nil)
		assert.False(t, ok)
	})

	t.Run("detached panel synthesis row left unbackfilled", func(t *testing.T) {
		job := storage.ReviewJob{JobType: storage.JobTypeSynthesis, GitRef: detachedSHA, RepoPath: repo.Path(), CommitID: &commitID}
		_, ok := backfillBranchValue(job, nil)
		assert.False(t, ok)
	})

	t.Run("reachable commit persists its branch", func(t *testing.T) {
		job := storage.ReviewJob{JobType: storage.JobTypeReview, GitRef: reachableSHA, RepoPath: repo.Path(), CommitID: &commitID}
		branch, ok := backfillBranchValue(job, nil)
		assert.True(t, ok)
		assert.Equal(t, "feature-x", branch)
	})

	t.Run("task job persists sentinel", func(t *testing.T) {
		job := storage.ReviewJob{JobType: storage.JobTypeTask, GitRef: "analyze"}
		branch, ok := backfillBranchValue(job, nil)
		assert.True(t, ok)
		assert.Equal(t, branchNone, branch)
	})

	t.Run("git command failure persists sentinel", func(t *testing.T) {
		job := storage.ReviewJob{JobType: storage.JobTypeReview, GitRef: detachedSHA, RepoPath: t.TempDir(), CommitID: &commitID}
		branch, ok := backfillBranchValue(job, nil)
		assert.True(t, ok)
		assert.Equal(t, branchNone, branch)
	})

	t.Run("locally missing repo persists sentinel instead of stranding the row", func(t *testing.T) {
		job := storage.ReviewJob{JobType: storage.JobTypeReview, GitRef: detachedSHA, RepoPath: "/nonexistent/repo", CommitID: &commitID}
		branch, ok := backfillBranchValue(job, nil)
		assert.True(t, ok)
		assert.Equal(t, branchNone, branch)
	})

	t.Run("remote job persists sentinel without lookup", func(t *testing.T) {
		job := storage.ReviewJob{JobType: storage.JobTypeReview, GitRef: detachedSHA, RepoPath: repo.Path(), CommitID: &commitID, SourceMachineID: testUUIDPtr("machine-b")}
		branch, ok := backfillBranchValue(job, testUUIDPtr("machine-a"))
		assert.True(t, ok)
		assert.Equal(t, branchNone, branch)
	})
}

// TestBranchMatchesFilterDetachedGroupsUnderNone pins filter identity: jobs
// rendered with the detached placeholder still match the (none) branch
// filter, agreeing with the branch picker's counts (#499 follow-up).
func TestBranchMatchesFilterDetachedGroupsUnderNone(t *testing.T) {
	t.Parallel()
	repo := testutil.InitTestRepo(t)
	repo.CommitFile("a.txt", "one", "first")
	repo.CheckoutDetached()
	sha := repo.CommitFile("a.txt", "two", "second, on detached HEAD")

	commitID := int64(9)
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.activeBranchFilter = branchNone

	detached := storage.ReviewJob{
		ID:       9,
		JobType:  storage.JobTypeReview,
		GitRef:   sha,
		RepoPath: repo.Path(),
		CommitID: &commitID,
	}
	assert.True(t, m.branchMatchesFilter(detached), "empty-branch detached job should match (none)")

	// A backfilled branchNone sentinel also groups under (none).
	detached.Branch = branchNone
	m2 := newModel(localhostEndpoint, withExternalIODisabled())
	m2.activeBranchFilter = branchNone
	assert.True(t, m2.branchMatchesFilter(detached), "backfilled branchNone job should match (none)")
}

// TestGetBranchForJobReachableFromBranch ensures the existing git name-rev
// fallback still wins over the new placeholder when the commit is in fact
// reachable from a local branch.
func TestGetBranchForJobReachableFromBranch(t *testing.T) {
	t.Parallel()
	repo := testutil.InitTestRepo(t)
	sha := repo.CommitFile("a.txt", "one", "first")

	commitID := int64(2)
	job := storage.ReviewJob{
		ID:       2,
		JobType:  storage.JobTypeReview,
		GitRef:   sha,
		RepoPath: repo.Path(),
		CommitID: &commitID,
	}

	m := newModel(localhostEndpoint, withExternalIODisabled())
	got := m.getBranchForJobCtx(t.Context(), job)

	assert.Equal(t, "main", got)
}

func TestGetBranchForJobLookupFailure(t *testing.T) {
	t.Parallel()
	repo := testutil.InitTestRepo(t)
	sha := repo.CommitFile("a.txt", "one", "first")
	commitID := int64(1)
	job := storage.ReviewJob{
		ID:       1,
		JobType:  storage.JobTypeReview,
		GitRef:   sha,
		RepoPath: repo.Path(),
		CommitID: &commitID,
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Time{})
	defer cancel()
	canceledCtx, cancelLookup := context.WithCancel(t.Context())
	cancelLookup()

	for _, tt := range []struct {
		name     string
		ctx      context.Context
		repoPath string
	}{
		{"deadline exceeded", ctx, repo.Path()},
		{"canceled", canceledCtx, repo.Path()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			m := newModel(localhostEndpoint, withExternalIODisabled())
			failedJob := job
			failedJob.RepoPath = tt.repoPath
			assert.Empty(m.getBranchForJobCtx(tt.ctx, failedJob))
			assert.NotContains(m.branchNames, job.ID, "failed lookups must remain retryable")

			assert.Equal("main", m.getBranchForJobCtx(t.Context(), job))
			assert.Equal("main", m.branchNames[job.ID])
		})
	}
}

func TestBranchLookupRunningTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("slow git wrapper uses a POSIX shell")
	}
	repo := testutil.InitTestRepo(t)
	sha := repo.CommitFile("a.txt", "one", "first")
	job := storage.ReviewJob{
		ID: 1, JobType: storage.JobTypeReview, GitRef: sha,
		RepoPath: repo.Path(), CommitID: new(int64(1)),
	}

	for _, name := range []string{"git", "queue", "backfill", "review"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			m := newModel(localhostEndpoint, withExternalIODisabled())
			started := filepath.Join(t.TempDir(), "started")
			t.Setenv("BRANCH_LOOKUP_STARTED", started)
			restore := testutil.MockBinaryInPath(t, "git", "#!/bin/sh\n: > \"$BRANCH_LOOKUP_STARTED\"\nexec sleep 30\n")
			t.Cleanup(restore)

			// These production deadlines kill a subprocess, which synctest cannot observe.
			switch name {
			case "git":
				branch, err := git.GetBranchName(job.RepoPath, job.GitRef)
				assert.Empty(branch)
				require.ErrorIs(t, err, context.DeadlineExceeded)
			case "queue":
				assert.Empty(m.getBranchForJob(job))
				assert.NotContains(m.branchNames, job.ID)
			case "backfill":
				branch, persist := backfillBranchValue(job, nil)
				assert.Empty(branch)
				assert.False(persist)
			case "review":
				assert.Empty(reviewBranchName(&job))
			}
			require.FileExists(t, started, "git must start before its deadline expires")

			restore()
			switch name {
			case "git":
				branch, err := git.GetBranchNameCtx(t.Context(), job.RepoPath, job.GitRef)
				require.NoError(t, err)
				assert.Equal("main", branch)
			case "queue":
				assert.Equal("main", m.getBranchForJobCtx(t.Context(), job))
			case "backfill":
				branch, persist := backfillBranchValue(job, nil)
				assert.Equal("main", branch)
				assert.True(persist)
			case "review":
				assert.Equal("main", reviewBranchName(&job))
			}
		})
	}
}

func TestGetBranchForJobPersistentFailureCached(t *testing.T) {
	t.Parallel()
	repo := testutil.InitTestRepo(t)
	sha := repo.CommitFile("a.txt", "one", "first")
	job := storage.ReviewJob{
		ID: 1, JobType: storage.JobTypeReview, GitRef: sha,
		RepoPath: t.TempDir(), CommitID: new(int64(1)),
	}
	m := newModel(localhostEndpoint, withExternalIODisabled())
	want := "(detached @ " + sha[:7] + ")"
	assert.Equal(t, want, m.getBranchForJob(job))
	assert.Equal(t, want, m.branchNames[job.ID])

	// A valid repo would resolve main if the next render retried the lookup.
	job.RepoPath = repo.Path()
	assert.Equal(t, want, m.getBranchForJob(job))
}
