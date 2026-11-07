package storage

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPullSessionParentsCommitAndRollbackWithJobs(t *testing.T) {
	t.Parallel()
	h := newSyncTestHelper(t)
	worker := NewSyncWorker(h.db, testSyncConfig())
	s, err := h.db.newPullSession(t.Context())
	require.NoError(t, err)
	defer s.close()
	s.budget = time.Hour
	var notifications int
	s.notify = func() { notifications++ }
	now := time.Now().UTC()
	job := PulledJob{
		UUID: testUUID("batch-first"), RepoIdentity: "https://example.com/batch.git",
		CommitSHA: "abc123", CommitTimestamp: now, GitRef: "first", Agent: "test",
		JobType: JobTypeReview, Status: "done", EnqueuedAt: now, UpdatedAt: now,
		SourceMachineID: testUUID("peer"),
	}
	require.NoError(t, worker.pullJob(t.Context(), s, job))
	second := job
	second.UUID = testUUID("batch-second")
	require.NoError(t, worker.pullJob(t.Context(), s, second))
	assert.Zero(t, notifications)
	var count int
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM repos WHERE identity = ?`, job.RepoIdentity).Scan(&count))
	assert.Zero(t, count, "parents remain private until commit")
	require.NoError(t, s.commit(t.Context()))
	assert.Equal(t, 1, notifications)
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM repos WHERE identity = ?`, job.RepoIdentity).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM commits WHERE sha = ?`, job.CommitSHA).Scan(&count))
	assert.Equal(t, 1, count, "rows in the same transaction reuse their parents")

	_, err = h.db.ExecContext(t.Context(), `CREATE TRIGGER fail_pull BEFORE INSERT ON review_jobs WHEN NEW.git_ref = 'fail' BEGIN SELECT RAISE(ABORT, 'injected row failure'); END`)
	require.NoError(t, err)
	job.UUID = testUUID("rollback-first")
	job.RepoIdentity = "https://example.com/rollback.git"
	job.CommitSHA = "def456"
	require.NoError(t, worker.pullJob(t.Context(), s, job))
	second = job
	second.UUID = testUUID("rollback-second")
	second.GitRef = "fail"
	require.ErrorContains(t, worker.pullJob(t.Context(), s, second), "injected row failure")
	assert.Equal(t, 1, notifications)
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM repos WHERE identity = ?`, job.RepoIdentity).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM commits WHERE sha = ?`, job.CommitSHA).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM review_jobs WHERE uuid IN (?, ?)`, job.UUID, second.UUID).Scan(&count))
	assert.Zero(t, count)
	second.GitRef = "retry"
	require.NoError(t, worker.pullJob(t.Context(), s, job))
	require.NoError(t, worker.pullJob(t.Context(), s, second))
	require.NoError(t, s.commit(t.Context()))
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM review_jobs WHERE uuid IN (?, ?)`, job.UUID, second.UUID).Scan(&count))
	assert.Equal(t, 2, count)
	assert.Equal(t, 2, notifications)
}

func TestPullSessionYieldsToQueuedWriter(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newSyncTestHelper(t)
		s, err := h.db.newPullSession(t.Context())
		require.NoError(t, err)
		defer s.close()
		s.budget = 0
		_, err = h.db.ExecContext(t.Context(), `CREATE TABLE pull_order (id INTEGER PRIMARY KEY, writer TEXT)`)
		require.NoError(t, err)
		require.NoError(t, s.begin(t.Context(), false))
		_, err = s.exec(t.Context(), `INSERT INTO pull_order (writer) VALUES (?)`, "pull-first")
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() {
			_, err := h.db.ExecContext(t.Context(), `INSERT INTO pull_order (writer) VALUES ('user')`)
			done <- err
		}()
		synctest.Wait()
		require.NoError(t, s.finishRow(t.Context(), true, nil))
		require.NoError(t, s.begin(t.Context(), false))
		_, err = s.exec(t.Context(), `INSERT INTO pull_order (writer) VALUES (?)`, "pull-second")
		require.NoError(t, err)
		require.NoError(t, s.finishRow(t.Context(), true, nil))
		require.NoError(t, <-done)
		var order string
		require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT group_concat(writer, ',') FROM (SELECT writer FROM pull_order ORDER BY id)`).Scan(&order))
		assert.Equal(t, "pull-first,user,pull-second", order)
	})
}

func TestPullSessionBudgetAndClose(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newSyncTestHelper(t)
		s, err := h.db.newPullSession(t.Context())
		require.NoError(t, err)
		defer s.close()
		s.budget = 5 * time.Millisecond
		require.NoError(t, s.begin(t.Context(), true))
		_, err = s.exec(t.Context(), `INSERT INTO sync_state (key, value) VALUES (?, 'value')`, "budget")
		require.NoError(t, err)
		time.Sleep(5 * time.Millisecond)
		require.NoError(t, s.finishRow(t.Context(), true, nil))
		value, err := h.db.GetSyncState("budget")
		require.NoError(t, err)
		assert.Equal(t, "value", value)
		require.NoError(t, s.begin(t.Context(), true))
		_, err = s.exec(t.Context(), `INSERT INTO sync_state (key, value) VALUES (?, 'value')`, "abandoned")
		require.NoError(t, err)
		s.close()
		value, err = h.db.GetSyncState("abandoned")
		require.NoError(t, err)
		assert.Empty(t, value)
		// Closing releases both the writer and attempt lock.
		require.NoError(t, h.db.UpsertPulledJob(PulledJob{
			UUID: testUUID("after-close"), GitRef: "main", Agent: "test", JobType: JobTypeReview,
			Status: "done", EnqueuedAt: time.Now(), UpdatedAt: time.Now(),
		}, h.repo.ID, nil))
	})
}

func TestPullSessionCanceledCommitRollsBack(t *testing.T) {
	t.Parallel()
	h := newSyncTestHelper(t)
	s, err := h.db.newPullSession(t.Context())
	require.NoError(t, err)
	defer s.close()
	require.NoError(t, s.begin(t.Context(), true))
	_, err = s.exec(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('canceled-pull', 'value')`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, s.commit(ctx), context.Canceled)
	value, err := h.db.GetSyncState("canceled-pull")
	require.NoError(t, err)
	assert.Empty(t, value)
	_, err = h.db.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('after-cancel', 'value')`)
	require.NoError(t, err)
}

func TestPullSessionIgnoredReviewKeepsPreviousResult(t *testing.T) {
	t.Parallel()
	h := newSyncTestHelper(t)
	worker := NewSyncWorker(h.db, testSyncConfig())
	s, err := h.db.newPullSession(t.Context())
	require.NoError(t, err)
	defer s.close()
	s.budget = time.Hour
	now := time.Now().UTC().Truncate(time.Microsecond)
	job := PulledJob{
		UUID: testUUID("ignored-review-parent"), RepoIdentity: "https://example.com/ignored.git",
		GitRef: "main", Agent: "test", JobType: JobTypeReview, Status: "done",
		EnqueuedAt: now, UpdatedAt: now, SourceMachineID: testUUID("peer"),
	}
	require.NoError(t, worker.pullJob(t.Context(), s, job))
	require.NoError(t, s.commit(t.Context()))
	review := PulledReview{
		UUID: testUUID("original-result"), JobUUID: job.UUID, Agent: "test",
		JobEnqueuedAt: now, JobSourceMachineID: job.SourceMachineID,
		StructuredOutput: reviewFixtureJSON("original finding"),
		CreatedAt:        now, UpdatedAt: now, UpdatedByMachineID: job.SourceMachineID,
	}
	require.NoError(t, worker.pullReview(t.Context(), s, review))
	require.NoError(t, s.commit(t.Context()))
	review.UUID = testUUID("ignored-result")
	review.StructuredOutput = nil
	review.CreatedAt = now.Add(time.Second)
	review.UpdatedAt = review.CreatedAt
	require.NoError(t, worker.pullReview(t.Context(), s, review))
	require.NoError(t, s.commit(t.Context()))
	var id string
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT uuid FROM reviews`).Scan(&id))
	assert.Equal(t, testUUID("original-result").String(), id)
}

func TestPullSessionCancelsAttemptOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	h := newSyncTestHelper(t)
	h.createPendingJob("active-pull")
	job := claimJob(t, h.db, "worker")
	require.NotNil(t, job)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release, err := h.db.StartJobAttempt(t.Context(), job, "worker", cancel)
	require.NoError(t, err)
	defer release()
	s, err := h.db.newPullSession(t.Context())
	require.NoError(t, err)
	defer s.close()
	incoming := PulledJob{
		UUID: *job.UUID, GitRef: job.GitRef, Agent: "test", JobType: JobTypeReview,
		Status: "done", EnqueuedAt: job.EnqueuedAt, UpdatedAt: time.Now().UTC(),
		SourceMachineID: *job.SourceMachineID,
	}
	require.NoError(t, s.begin(t.Context(), true))
	changed, err := s.upsertPulledJob(t.Context(), incoming, h.repo.ID, nil)
	require.NoError(t, err)
	assert.True(t, changed)
	require.NoError(t, ctx.Err())
	s.rollback()
	require.NoError(t, ctx.Err(), "a rolled-back pull must keep the attempt running")
	require.NoError(t, s.begin(t.Context(), true))
	changed, err = s.upsertPulledJob(t.Context(), incoming, h.repo.ID, nil)
	require.NoError(t, err)
	assert.True(t, changed)
	require.NoError(t, s.commit(t.Context()))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	stored, err := h.db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusDone, stored.Status)
}
