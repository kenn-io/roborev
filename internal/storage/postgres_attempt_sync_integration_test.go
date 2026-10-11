//go:build postgres

package storage

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func importedAttemptNodes(t *testing.T) (*integrationEnv, testNode, testNode, int64, int64) {
	t.Helper()
	env := newIntegrationEnv(t)
	a := env.setupNode("source", "attempt-repository", "1h")
	b := env.setupNode("rerun", "attempt-repository", "1h")
	job, _ := createCompletedReview(t, a.DB, a.Repo.ID, "abc123", "Author", "Subject", "original prompt", "Original review.")
	_, err := a.Worker.SyncNow()
	require.NoError(t, err)
	_, err = b.Worker.SyncNow()
	require.NoError(t, err)
	var importedID int64
	require.NoError(t, b.DB.QueryRow(`SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&importedID))
	return env, a, b, job.ID, importedID
}

func startImportedAttempt(t *testing.T, node testNode, id int64, prompt string) {
	t.Helper()
	require.NoError(t, node.DB.ReenqueueJob(id, ReenqueueOpts{}))
	job, err := node.DB.ClaimJob("attempt-worker")
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, id, job.ID)
	require.NoError(t, node.DB.SaveJobPrompt(id, prompt))
}

func TestIntegrationImportedRerunWithoutReview(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	for _, tc := range []struct {
		name   string
		status JobStatus
		prompt string
	}{
		{"failed", JobStatusFailed, "unsuccessful attempt prompt"},
		{"canceled", JobStatusCanceled, "unsuccessful attempt prompt"},
		{"failed before prompt", JobStatusFailed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			_, a, b, sourceID, importedID := importedAttemptNodes(t)
			startImportedAttempt(t, b, importedID, tc.prompt)
			if status == JobStatusFailed {
				changed, err := b.DB.FailJob(importedID, "attempt-worker", "Synthetic failure")
				require.NoError(t, err)
				require.True(t, changed)
			} else {
				require.NoError(t, b.DB.CancelJob(importedID))
				changed, err := b.DB.ReleaseCanceledJob(importedID, "attempt-worker")
				require.NoError(t, err)
				require.True(t, changed)
			}
			_, err := b.Worker.SyncNow()
			require.NoError(t, err)
			_, err = a.Worker.SyncNow()
			require.NoError(t, err)
			for i, node := range []testNode{a, b} {
				id := []int64{sourceID, importedID}[i]
				job, err := node.DB.GetJobByID(id)
				require.NoError(t, err)
				assert.Equal(t, status, job.Status)
				assert.Equal(t, tc.prompt, job.Prompt)
				_, err = node.DB.GetReviewByJobID(id)
				assert.ErrorIs(t, err, sql.ErrNoRows)
			}
		})
	}
}

func TestIntegrationOlderAttemptFinishesLast(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	env, a, b, sourceID, importedID := importedAttemptNodes(t)
	startImportedAttempt(t, a, sourceID, "obsolete overlapping attempt")
	startImportedAttempt(t, b, importedID, "current attempt prompt")
	require.NoError(t, completeReviewFixture(b.DB, importedID, "test", "current attempt prompt", "Current review."))
	_, err := b.Worker.SyncNow()
	require.NoError(t, err)
	// The earlier executor finishes and uploads after the newer attempt.
	require.NoError(t, completeReviewFixture(a.DB, sourceID, "test", "obsolete overlapping attempt", "Obsolete review."))
	_, err = a.Worker.SyncNow()
	require.NoError(t, err)
	_, err = b.Worker.SyncNow()
	require.NoError(t, err)
	c := env.setupNode("observer", "attempt-repository", "1h")
	jobs, err := c.DB.ListJobs("", "", 10, 0)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "current attempt prompt", jobs[0].Prompt)
	review, err := c.DB.GetReviewByJobID(jobs[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "Current review.", review.StructuredOutput["summary"])
}

func TestIntegrationReviewWaitsForProducingAttempt(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	_, a, b, sourceID, importedID := importedAttemptNodes(t)
	startImportedAttempt(t, b, importedID, "current prompt")
	require.NoError(t, completeReviewFixture(b.DB, importedID, "test", "current prompt", "Current review."))
	_, err := b.Worker.SyncNow()
	require.NoError(t, err)
	// Skip the job phase to model a newer attempt arriving between this
	// client's job and review queries. Its review cursor must not advance.
	jobCursor, err := a.DB.GetSyncState(SyncStateLastJobCursor)
	require.NoError(t, err)
	reviewCursor, err := a.DB.GetSyncState(SyncStateLastReviewCursor)
	require.NoError(t, err)
	require.NoError(t, a.DB.SetSyncState(SyncStateLastJobCursor, time.Now().Add(24*time.Hour).Format(time.RFC3339Nano)+" 0"))
	_, err = a.Worker.SyncNow()
	require.ErrorContains(t, err, "review attempt is newer than local job")
	after, err := a.DB.GetSyncState(SyncStateLastReviewCursor)
	require.NoError(t, err)
	assert.Equal(t, reviewCursor, after)
	require.NoError(t, a.DB.SetSyncState(SyncStateLastJobCursor, jobCursor))
	_, err = a.Worker.SyncNow()
	require.NoError(t, err)
	review, err := a.DB.GetReviewByJobID(sourceID)
	require.NoError(t, err)
	assert.Equal(t, "Current review.", review.StructuredOutput["summary"])
}
