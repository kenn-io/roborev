package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// TestGetBranchForJobDetachedHead covers the queue view's branch fallback
// (#499): a review job with no stored branch, enqueued from a commit made on
// top of a detached HEAD (e.g. mid git-bisect), has no branch reachable via
// git name-rev either. Instead of caching and returning "", getBranchForJob
// should surface a "(detached @ <sha>)" placeholder.
func TestGetBranchForJobDetachedHead(t *testing.T) {
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
	got := m.getBranchForJob(job)

	assert.Equal(t, "(detached @ "+sha[:7]+")", got)

	// Result is cached rather than re-derived on the next call.
	assert.Equal(t, got, m.branchNames[job.ID])
}

// TestGetBranchForJobBranchNoneSentinel covers backfilled jobs whose stored
// branch is the branchNone sentinel (#499 follow-up): the detached
// placeholder is only shown after a local lookup verifies it; remote or
// repo-less jobs keep the sentinel.
func TestGetBranchForJobBranchNoneSentinel(t *testing.T) {
	repo := testutil.InitTestRepo(t)
	repo.CommitFile("a.txt", "one", "first")
	repo.CheckoutDetached()
	sha := repo.CommitFile("a.txt", "two", "second, on detached HEAD")

	commitID := int64(1)
	base := storage.ReviewJob{
		ID:       7,
		JobType:  storage.JobTypeReview,
		Branch:   branchNone,
		GitRef:   sha,
		RepoPath: repo.Path(),
		CommitID: &commitID,
	}

	t.Run("verified locally shows placeholder and caches", func(t *testing.T) {
		m := newModel(localhostEndpoint, withExternalIODisabled())
		got := m.getBranchForJob(base)
		assert.Equal(t, "(detached @ "+sha[:7]+")", got)
		assert.Equal(t, got, m.branchNames[base.ID])
	})

	t.Run("remote job keeps sentinel, uncached", func(t *testing.T) {
		m := newModel(localhostEndpoint, withExternalIODisabled())
		m.status.MachineID = "machine-a"
		job := base
		job.SourceMachineID = "machine-b"
		assert.Equal(t, branchNone, m.getBranchForJob(job))
		assert.NotContains(t, m.branchNames, job.ID)
	})

	t.Run("missing repo keeps sentinel, uncached", func(t *testing.T) {
		m := newModel(localhostEndpoint, withExternalIODisabled())
		job := base
		job.RepoPath = "/nonexistent/repo"
		assert.Equal(t, branchNone, m.getBranchForJob(job))
		assert.NotContains(t, m.branchNames, job.ID)
	})
}

// TestBranchMatchesFilterDetachedGroupsUnderNone pins filter identity: jobs
// rendered with the detached placeholder still match the (none) branch
// filter, agreeing with the branch picker's counts (#499 follow-up).
func TestBranchMatchesFilterDetachedGroupsUnderNone(t *testing.T) {
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

	detached.Branch = branchNone
	// Fresh model so the cached display label from the first call is not reused.
	m2 := newModel(localhostEndpoint, withExternalIODisabled())
	m2.activeBranchFilter = branchNone
	assert.True(t, m2.branchMatchesFilter(detached), "backfilled branchNone detached job should match (none)")
}

// TestGetBranchForJobReachableFromBranch ensures the existing git name-rev
// fallback still wins over the new placeholder when the commit is in fact
// reachable from a local branch.
func TestGetBranchForJobReachableFromBranch(t *testing.T) {
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
	got := m.getBranchForJob(job)

	assert.NotEmpty(t, got)
	assert.NotContains(t, got, "detached")
}
