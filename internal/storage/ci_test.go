package storage

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAgent  = "codex"
	testReview = "security"
)

func assertEq[T comparable](t *testing.T, name string, got, want T) {
	t.Helper()
	assert.Equalf(t, want, got, "assertion failed for %s: got=%v, want=%v", name, got, want)
}

func mustEnqueueReviewJob(t *testing.T, db *DB, repoID int64, gitRef, agent, reviewType string) *ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repoID, GitRef: gitRef, Agent: agent, ReviewType: reviewType,
	})
	require.NoError(t, err, "EnqueueJob")
	return job
}

func TestCancelJob_ReturnsErrNoRowsForTerminalJobs(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-cancel")
	require.NoError(t, err, "GetOrCreateRepo")

	t.Run("TerminalJob_ReturnsErrNoRows", func(t *testing.T) {
		job := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
		setJobStatus(t, db, job.ID, JobStatusDone)

		err := db.CancelJob(job.ID)
		require.ErrorIs(t, err, sql.ErrNoRows, "expected sql.ErrNoRows, got: %v", err)
	})

	t.Run("QueuedJob_Succeeds", func(t *testing.T) {
		job := mustEnqueueReviewJob(t, db, repo.ID, "c..d", testAgent, testReview)
		require.NoError(t, db.CancelJob(job.ID), "CancelJob on queued job")

		var status string
		require.NoError(t, db.QueryRow(`SELECT status FROM review_jobs WHERE id = ?`, job.ID).Scan(&status), "query status")
		assertEq(t, "status", status, "canceled")
	})
}

func TestCancelJobWithError(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-cancel-err")
	require.NoError(t, err)

	t.Run("sets error on cancel", func(t *testing.T) {
		job := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
		err := db.CancelJobWithError(job.ID, "timeout: posted early")
		require.NoError(t, err)

		updated, err := db.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, JobStatusCanceled, updated.Status)
		assert.Equal(t, "timeout: posted early", updated.Error)
	})

	t.Run("returns ErrNoRows for terminal job", func(t *testing.T) {
		job := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
		setJobStatus(t, db, job.ID, JobStatusDone)

		err := db.CancelJobWithError(job.ID, "timeout: posted early")
		require.ErrorIs(t, err, sql.ErrNoRows)
	})
}
