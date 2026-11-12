package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateCIBranchReviewRunReviewsACommitOnce(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-branch-review-once")
	members := []EnqueueOpts{{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test"}}
	synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "parent..landed", Agent: "test"}

	created, synth, err := db.CreateCIBranchReviewRun("o/r", "main", "landed", members, synthesis)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(JobSourceCIBranch, synth.Source)

	again, _, err := db.CreateCIBranchReviewRun("o/r", "main", "landed", members, synthesis)
	require.NoError(t, err)
	assert.False(again, "a second poll of the same commit must not queue another review")

	jobs, err := db.ListJobs("", "", 100, 0)
	require.NoError(t, err)
	assert.Len(jobs, 2, "one member and one synthesis job")
	for _, job := range jobs {
		assert.Equal(JobSourceCIBranch, job.Source)
	}

	cursor, err := db.GetCIBranchCursor("o/r")
	require.NoError(t, err)
	require.NotNil(t, cursor)
	assert.Equal("landed", cursor.HeadSHA)

	review, err := db.GetCIBranchReviewBySynthesisJobID(synth.ID)
	require.NoError(t, err)
	require.NotNil(t, review)
	assert.Equal("landed", review.SHA)
}

func TestCIBranchReviewStatusIsSentOnce(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	require.NoError(t, db.RecordCIBranchCommitOutcome("o/r", "main", "landed", "error", "could not queue"))
	review, err := db.GetCIBranchReview("o/r", "landed")
	require.NoError(t, err)
	require.NotNil(t, review)

	claimed, err := db.ClaimCIBranchReviewStatus(review.ID, time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	again, err := db.ClaimCIBranchReviewStatus(review.ID, time.Minute)
	require.NoError(t, err)
	assert.False(again, "a claimed status is not sent by a second poller")

	require.NoError(t, db.FinishCIBranchReviewStatus(review.ID, false))
	retry, err := db.ClaimCIBranchReviewStatus(review.ID, time.Minute)
	require.NoError(t, err)
	assert.True(retry, "a failed send is retried")
	require.NoError(t, db.FinishCIBranchReviewStatus(review.ID, true))
	done, err := db.ClaimCIBranchReviewStatus(review.ID, time.Minute)
	require.NoError(t, err)
	assert.False(done, "a sent status is not sent again")
}
