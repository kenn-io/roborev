package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetSummaryIncludesCost(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/summary-cost")
	commit := createCommit(t, db, repo.ID, "sum-sha")
	job := enqueueJob(t, db, repo.ID, commit.ID, "sum-sha")
	setJobStatus(t, db, job.ID, JobStatusDone)
	seedCost(t, db, job.ID, `{"cost_usd":0.42,"has_cost":true}`)

	// Priced, eligible job enqueued before the window. It must be excluded by
	// Since; if Since propagation were dropped, it would inflate the totals.
	old := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, "old-sha").ID, "old-sha")
	setJobStatus(t, db, old.ID, JobStatusDone)
	_, err := db.Exec(`UPDATE review_jobs SET enqueued_at = datetime('now','-48 hours') WHERE id = ?`, old.ID)
	require.NoError(t, err)
	seedCost(t, db, old.ID, `{"cost_usd":9.99,"has_cost":true}`)

	s, err := db.GetSummary(SummaryOptions{
		RepoPath: repo.RootPath,
		Since:    time.Now().Add(-24 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(1, s.Cost.JobsTotal)
	assert.Equal(1, s.Cost.JobsWithCost)
	assert.InDelta(0.42, s.Cost.TotalUSD, 0.0001)
	assert.True(s.Cost.Complete)
}
