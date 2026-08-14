package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetLatestLogicalReviewJobOrdersMixedEnqueueTimestampFormats(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	repo, jobs := seedJobs(t, db, "/tmp/mixed-projection-order", 2)

	_, err := db.Exec(
		`UPDATE review_jobs SET branch = ?, enqueued_at = ? WHERE id = ?`,
		"main", "2026-08-14T08:00:00Z", jobs[0].ID,
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`UPDATE review_jobs SET branch = ?, enqueued_at = ? WHERE id = ?`,
		"main", "2026-08-14 09:00:00", jobs[1].ID,
	)
	require.NoError(t, err)

	latest, err := db.GetLatestLogicalReviewJob(repo.RootPath, "main")
	require.NoError(t, err)
	assert.Equal(t, jobs[1].ID, latest.ID)
}
