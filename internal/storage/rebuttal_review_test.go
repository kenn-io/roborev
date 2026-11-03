package storage

import (
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRebuttalReviewVersionRoundTrip(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	repo := createRepo(t, db, t.TempDir())
	version := &ReviewVersion{ID: 42, UUID: uuid.New()}

	recorded, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "abc1234", Agent: "test", JobType: JobTypeTask,
		Source: JobSourceRebuttal, Prompt: "re-check", RebuttalReview: version,
	})
	require.NoError(t, err)
	plain, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "abc1234", Agent: "test", JobType: JobTypeTask, Prompt: "task"})
	require.NoError(t, err)

	got, err := db.RebuttalReviewVersion(t.Context(), recorded.ID)
	require.NoError(t, err)
	assert.Equal(t, version, got)
	got, err = db.RebuttalReviewVersion(t.Context(), plain.ID)
	require.NoError(t, err)
	assert.Nil(t, got, "jobs that recorded no version report none")
}
