package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepoOperations(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Create repo
	repo, err := db.GetOrCreateRepo("/tmp/test-repo")
	require.NoError(t, err, "GetOrCreateRepo failed: %v")

	assert.NotEqual(t, 0, repo.ID)
	assert.Equal(t, "test-repo", repo.Name)

	// Get same repo again (should return existing)
	repo2, err := db.GetOrCreateRepo("/tmp/test-repo")
	require.NoError(t, err, "GetOrCreateRepo (second call) failed: %v")

	assert.Equal(t, repo2.ID, repo.ID)
}

func TestCommitOperations(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")

	// Create commit
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123def456", "Test Author", "Test commit", time.Now())
	require.NoError(t, err, "GetOrCreateCommit failed: %v")

	assert.NotEqual(t, 0, commit.ID)
	assert.Equal(t, "abc123def456", commit.SHA)

	// Get by SHA
	found, err := db.GetCommitBySHA("abc123def456")
	require.NoError(t, err, "GetCommitBySHA failed: %v")

	assert.Equal(t, found.ID, commit.ID)
}

func TestBranchPersistence(t *testing.T) {
	t.Run("EnqueueJob stores branch", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/branch-test-repo")
		commit := createCommit(t, db, repo.ID, "branch123")

		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "branch123", Branch: "feature/test-branch", Agent: "codex"})
		require.NoError(t, err, "EnqueueJob failed: %v")

		assert.Equal(t, "feature/test-branch", job.Branch)
	})

	t.Run("GetJobByID returns branch", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/branch-test-repo")
		commit := createCommit(t, db, repo.ID, "branch123")

		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "branch456", Branch: "main", Agent: "codex"})
		require.NoError(t, err, "EnqueueJob failed: %v")

		fetched, err := db.GetJobByID(job.ID)
		require.NoError(t, err, "GetJobByID failed: %v")

		assert.Equal(t, "main", fetched.Branch)
	})

	t.Run("ListJobs returns branch", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/branch-test-repo")
		commit := createCommit(t, db, repo.ID, "branch123")

		_, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "branch789", Branch: "develop", Agent: "codex"})
		require.NoError(t, err, "EnqueueJob failed: %v")

		jobs, err := db.ListJobs("", "", 100, 0)
		require.NoError(t, err, "ListJobs failed: %v")

		require.Len(t, jobs, 1)
		assert.Equal(t, "develop", jobs[0].Branch)
	})

	t.Run("ClaimJob returns branch", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/branch-test-repo")
		commit := createCommit(t, db, repo.ID, "branch123")

		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "branchclaim", Branch: "release/v1", Agent: "codex"})
		require.NoError(t, err, "EnqueueJob failed: %v")

		claimed, err := db.ClaimJob("test-worker")
		require.NoError(t, err, "ClaimJob failed: %v")

		require.NotNil(t, claimed)
		assert.Equal(t, job.ID, claimed.ID)
		assert.Equal(t, "release/v1", claimed.Branch)
	})

	t.Run("empty branch is allowed", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/branch-test-repo")
		commit := createCommit(t, db, repo.ID, "branch123")

		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "nobranch", Agent: "codex"})
		require.NoError(t, err, "EnqueueJob with empty branch failed: %v")

		assert.Empty(t, job.Branch)
	})

	t.Run("UpdateJobBranch backfills empty branch", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/branch-test-repo")
		commit := createCommit(t, db, repo.ID, "branch123")

		// Create job with empty branch
		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "updatebranch", Agent: "codex"})
		require.NoError(t, err, "EnqueueJob failed: %v")

		assert.Empty(t, job.Branch)

		// Update the branch
		rowsAffected, err := db.UpdateJobBranch(job.ID, "feature/backfilled")
		require.NoError(t, err, "UpdateJobBranch failed: %v")

		assert.EqualValues(t, 1, rowsAffected)

		// Verify the branch was updated
		fetched, err := db.GetJobByID(job.ID)
		require.NoError(t, err, "GetJobByID failed: %v")

		assert.Equal(t, "feature/backfilled", fetched.Branch)
	})

	t.Run("UpdateJobBranch does not overwrite existing branch", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/branch-test-repo")
		commit := createCommit(t, db, repo.ID, "branch123")

		// Create job with existing branch
		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "nooverwrite", Branch: "original-branch", Agent: "codex"})
		require.NoError(t, err, "EnqueueJob failed: %v")

		// Try to update - should not change existing branch
		rowsAffected, err := db.UpdateJobBranch(job.ID, "new-branch")
		require.NoError(t, err, "UpdateJobBranch failed: %v")

		assert.EqualValues(t, 0, rowsAffected)

		// Verify the branch was NOT changed
		fetched, err := db.GetJobByID(job.ID)
		require.NoError(t, err, "GetJobByID failed: %v")

		assert.Equal(t, "original-branch", fetched.Branch)
	})
}

func TestRepoIdentity(t *testing.T) {
	t.Run("sets identity on create", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo, err := db.GetOrCreateRepo("/tmp/identity-test", "git@github.com:foo/bar.git")
		require.NoError(t, err, "GetOrCreateRepo failed: %v")

		assert.Equal(t, "git@github.com:foo/bar.git", repo.Identity)

		// Verify it persists
		repo2, err := db.GetOrCreateRepo("/tmp/identity-test")
		require.NoError(t, err, "GetOrCreateRepo (second call) failed: %v")

		assert.Equal(t, "git@github.com:foo/bar.git", repo2.Identity)
	})

	t.Run("backfills identity when not set", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		// Create repo without identity
		repo1, err := db.GetOrCreateRepo("/tmp/backfill-test")
		require.NoError(t, err, "GetOrCreateRepo failed: %v")

		assert.Empty(t, repo1.Identity)

		// Call again with identity - should backfill
		repo2, err := db.GetOrCreateRepo("/tmp/backfill-test", "git@github.com:test/backfill.git")
		require.NoError(t, err, "GetOrCreateRepo with identity failed: %v")

		assert.Equal(t, "git@github.com:test/backfill.git", repo2.Identity)
	})

	t.Run("does not overwrite existing identity", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		// Create repo with identity
		_, err := db.GetOrCreateRepo("/tmp/no-overwrite-test", "original-identity")
		require.NoError(t, err, "GetOrCreateRepo failed: %v")

		// Call again with different identity - should NOT overwrite
		repo2, err := db.GetOrCreateRepo("/tmp/no-overwrite-test", "new-identity")
		require.NoError(t, err, "GetOrCreateRepo with new identity failed: %v")

		assert.Equal(t, "original-identity", repo2.Identity)
	})

	t.Run("multiple clones with same identity allowed", func(t *testing.T) {
		// This tests the fix for https://github.com/roborev-dev/roborev/issues/131
		// Multiple clones of the same repo (e.g., ~/project-1 and ~/project-2 both
		// cloned from the same remote) should be allowed and share the same identity.
		db := openTestDB(t)
		defer db.Close()

		sharedIdentity := "git@github.com:org/shared-repo.git"

		// Create first clone
		repo1, err := db.GetOrCreateRepo("/tmp/clone-1", sharedIdentity)
		require.NoError(t, err, "GetOrCreateRepo for clone-1 failed: %v")

		assert.Equal(t, repo1.Identity, sharedIdentity)

		// Create second clone with same identity - should succeed (was failing before fix)
		repo2, err := db.GetOrCreateRepo("/tmp/clone-2", sharedIdentity)
		require.NoError(t, err, "GetOrCreateRepo for clone-2 failed: %v (multiple clones with same identity should be allowed)")

		assert.Equal(t, repo2.Identity, sharedIdentity)

		// Verify they are different repos
		assert.NotEqual(t, repo1.ID, repo2.ID)
		assert.NotEqual(t, repo1.RootPath, repo2.RootPath, "Expected different root paths")

		// Verify both repos exist and can be retrieved
		repos, err := db.ListRepos()
		require.NoError(t, err, "ListRepos failed: %v")

		foundClone1, foundClone2 := false, false
		for _, r := range repos {
			if r.ID == repo1.ID {
				foundClone1 = true
			}
			if r.ID == repo2.ID {
				foundClone2 = true
			}
		}
		assert.False(t, !foundClone1 || !foundClone2)
	})
}

func TestDuplicateSHAHandling(t *testing.T) {
	t.Run("same SHA in different repos creates separate commits", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		// Create two repos
		repo1, _ := db.GetOrCreateRepo("/tmp/sha-test-1")
		repo2, _ := db.GetOrCreateRepo("/tmp/sha-test-2")

		// Create commits with same SHA in different repos
		commit1, err := db.GetOrCreateCommit(repo1.ID, "abc123", "Author1", "Subject1", time.Now())
		require.NoError(t, err, "GetOrCreateCommit for repo1 failed: %v")

		commit2, err := db.GetOrCreateCommit(repo2.ID, "abc123", "Author2", "Subject2", time.Now())
		require.NoError(t, err, "GetOrCreateCommit for repo2 failed: %v")

		// Should be different commits
		assert.NotEqual(t, commit1.ID, commit2.ID)
		assert.Equal(t, commit1.RepoID, repo1.ID)
		assert.Equal(t, commit2.RepoID, repo2.ID)
	})

	t.Run("GetCommitBySHA returns error when ambiguous", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		// Create two repos with same SHA
		repo1, _ := db.GetOrCreateRepo("/tmp/ambiguous-1")
		repo2, _ := db.GetOrCreateRepo("/tmp/ambiguous-2")

		db.GetOrCreateCommit(repo1.ID, "ambiguous-sha", "Author", "Subject", time.Now())
		db.GetOrCreateCommit(repo2.ID, "ambiguous-sha", "Author", "Subject", time.Now())

		// GetCommitBySHA should fail when ambiguous
		_, err := db.GetCommitBySHA("ambiguous-sha")
		require.Error(t, err)
	})

	t.Run("GetCommitByRepoAndSHA returns correct commit", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		// Create two repos with same SHA
		repo1, _ := db.GetOrCreateRepo("/tmp/repo-and-sha-1")
		repo2, _ := db.GetOrCreateRepo("/tmp/repo-and-sha-2")

		commit1, _ := db.GetOrCreateCommit(repo1.ID, "same-sha", "Author1", "Subject1", time.Now())
		commit2, _ := db.GetOrCreateCommit(repo2.ID, "same-sha", "Author2", "Subject2", time.Now())

		// GetCommitByRepoAndSHA should return correct commit for each repo
		found1, err := db.GetCommitByRepoAndSHA(repo1.ID, "same-sha")
		require.NoError(t, err, "GetCommitByRepoAndSHA for repo1 failed: %v")

		assert.Equal(t, found1.ID, commit1.ID)

		found2, err := db.GetCommitByRepoAndSHA(repo2.ID, "same-sha")
		require.NoError(t, err, "GetCommitByRepoAndSHA for repo2 failed: %v")

		assert.Equal(t, found2.ID, commit2.ID)
	})
}
