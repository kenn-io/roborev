package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimPrioritizesReviewJobsOverScheduledTasks(t *testing.T) {
	db, repo := setupDBAndRepo(t, "scheduled-queue")
	scheduled, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, Agent: "test", Prompt: "scheduled", JobType: JobTypeTask,
		Source: JobSourceScheduled,
	})
	require.NoError(t, err)
	review, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, Agent: "test", GitRef: "abc", JobType: JobTypeReview,
	})
	require.NoError(t, err)

	claimed, err := db.ClaimJob("queue-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, review.ID, claimed.ID)

	_, err = db.FailJob(review.ID, "queue-worker", "done")
	require.NoError(t, err)
	claimed, err = db.ClaimJob("queue-worker-2")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, scheduled.ID, claimed.ID)
}
