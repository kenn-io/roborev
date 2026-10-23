package prompt

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestKataRepliesReachFixAndPlanningWithoutChangingReview(t *testing.T) {
	review := &storage.Review{JobID: 17, Output: "A finding that remains unverified."}
	const claims = "Kata attributed claims: reviewer/validator refute with actual test evidence."
	b := NewBuilder(nil).ForRepo(t.TempDir(), 0).WithKataReplies(func(_ context.Context, got *storage.Review) string {
		assert.Equal(t, int64(17), got.JobID)
		return claims
	})
	address, err := b.BuildAddressPrompt(review, nil, "")
	require.NoError(t, err)
	plan, err := b.BuildPlanPrompt(review, nil, "")
	require.NoError(t, err)
	assert.Contains(t, address, claims)
	assert.Contains(t, plan, claims)
	assert.Equal(t, "A finding that remains unverified.", review.Output)
}

func TestNextLocalReviewReceivesMappedPriorReplies(t *testing.T) {
	repoPath, commits := setupTestRepo(t)
	db, repoID := setupDBWithCommits(t, repoPath, commits)
	testutil.CreateCompletedReview(t, db, repoID, commits[2], "test", "Original finding remains a claim.")
	const evidence = "The refutation reproducer still fails; validate before changing code."
	var sourceJobs []int64
	b := NewBuilderWithConfig(db, &config.Config{DefaultMaxPromptSize: 1}).ForRepo(repoPath, repoID).
		WithKataReplies(func(_ context.Context, source *storage.Review) string {
			sourceJobs = append(sourceJobs, source.JobID)
			return evidence
		})
	prepared, err := b.BuildWithSnapshot(commits[5], 5, "test", "", "", nil)
	require.NoError(t, err)
	if prepared.Cleanup != nil {
		t.Cleanup(prepared.Cleanup)
	}
	require.NotEmpty(t, prepared.FilePath, "complete prompt should use the normal file handoff")
	complete, err := os.ReadFile(prepared.FilePath)
	require.NoError(t, err)
	assert.Contains(t, string(complete), evidence)
	assert.Contains(t, string(complete), "Original finding remains a claim.")
	assert.NotEmpty(t, sourceJobs, "the next review must fetch prior replies, not only a fix")
}

func TestPriorRangeReviewDocumentIncludesAttributedReplies(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	base := testutil.GetHeadSHA(t, repo.Path())
	first := repo.CommitFile("first.go", "package first\n", "first")
	second := repo.CommitFile("second.go", "package second\n", "second")
	db := testutil.OpenTestDB(t)
	dbRepo, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	createCompletedRangeReview(t, db, dbRepo.ID, base+".."+first, storage.JobTypeRange, "Original range finding")
	const claims = "Reviewer/validator refute: evidence <untrusted> remains visible."
	b := NewBuilder(db).ForRepo(repo.Path(), dbRepo.ID).WithKataReplies(func(_ context.Context, review *storage.Review) string {
		return claims
	})
	prebuilt, err := b.Build(base+".."+second, 2, "test", "", "")
	require.NoError(t, err)
	prepared, err := b.PreparePriorRangeReviewsSnapshot(prebuilt, base+".."+second, 2, SnapshotTarget{})
	require.NoError(t, err)
	if prepared.Cleanup != nil {
		t.Cleanup(prepared.Cleanup)
	}
	_, reviews := readPriorRangeReviewsDocument(t, prepared.Prompt)
	require.Len(t, reviews, 1)
	assert.Contains(t, reviews[0].Output, claims)
}
