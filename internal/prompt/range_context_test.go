package prompt

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestBuildRangePrompt_IncludesPriorSameBaseRangeReview(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	base := testutil.GetHeadSHA(t, repo.Path())
	first := repo.CommitFile("first.go", "package first\n", "first")
	second := repo.CommitFile("second.go", "package second\n", "second")
	third := repo.CommitFile("third.go", "package third\n", "third")
	fourth := repo.CommitFile("fourth.go", "package fourth\n", "fourth")

	db := testutil.OpenTestDB(t)
	dbRepo, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	createCompletedRangeReview(t, db, dbRepo.ID, base+".."+second, storage.JobTypeRange, "prior range review")
	createCompletedRangeReview(t, db, dbRepo.ID, base+".."+first, storage.JobTypeSynthesis, "prior synthesis review")
	createCompletedRangeReview(t, db, dbRepo.ID, "other-base.."+second, storage.JobTypeRange, "unrelated review")
	createCompletedRangeReview(t, db, dbRepo.ID, base+".."+fourth, storage.JobTypeRange, "out of range review")
	for i := range 40 {
		createCompletedRangeReview(t, db, dbRepo.ID,
			fmt.Sprintf("unrelated-base-%d..unrelated-end-%d", i, i),
			storage.JobTypeRange, fmt.Sprintf("unrelated recent review %d", i))
	}

	prompt, err := NewBuilder(db).ForRepo(repo.Path(), dbRepo.ID).Build(
		base+".."+third, 2, "test", "", "",
	)
	require.NoError(t, err)

	assert.Contains(t, prompt, "## Prior Same-Base Range Reviews")
	assert.Contains(t, prompt, "prior range review")
	assert.Contains(t, prompt, "prior synthesis review")
	assert.Contains(t, prompt, base[:7]+".."+second[:7])
	assert.Contains(t, prompt, base[:7]+".."+first[:7])
	assert.NotContains(t, prompt, "unrelated review")
	assert.NotContains(t, prompt, "out of range review")

	withoutAddedRanges, err := NewBuilder(db).ForRepo(repo.Path(), dbRepo.ID).Build(
		base+".."+third, 0, "test", "", "",
	)
	require.NoError(t, err)
	assert.NotContains(t, withoutAddedRanges, "prior range review")
}

func createCompletedRangeReview(t *testing.T, db *storage.DB, repoID int64, ref, jobType, output string) {
	t.Helper()
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:  repoID,
		GitRef:  ref,
		Agent:   "test",
		JobType: jobType,
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("range-context-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", output))
}
