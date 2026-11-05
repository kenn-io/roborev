package storage

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserWritesQueueBehindPullTransaction(t *testing.T) {
	t.Parallel()
	h := newSyncTestHelper(t)
	job := h.createCompletedJob("user-write")
	pulled := PulledJob{
		UUID: testUUID("continuous-pull"), GitRef: "pulled-ref", Agent: "test",
		JobType: JobTypeReview, Status: string(JobStatusDone),
		EnqueuedAt: time.Now().UTC(), SourceMachineID: testUUID("peer"),
	}
	ctx, cancel := context.WithCancel(t.Context())
	var commits atomic.Int64
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			pulled.UpdatedAt = time.Now().UTC()
			if err := h.db.UpsertPulledJob(pulled, h.repo.ID, nil); err != nil {
				done <- err
				return
			}
			if commits.Add(1) == 1 {
				close(started)
			}
		}
		done <- nil
	}()
	defer func() {
		cancel()
		require.NoError(t, <-done)
	}()
	select {
	case <-started:
	case err := <-done:
		// Leave the result for cleanup, including if the pull fails at startup.
		done <- err
		require.NoError(t, err)
	}
	for i := range 20 {
		before := commits.Load()
		switch i % 3 {
		case 0:
			_, err := h.db.EnqueueJob(EnqueueOpts{RepoID: h.repo.ID, GitRef: "queued", Agent: "test"})
			require.NoError(t, err)
		case 1:
			_, err := h.db.AddCommentToJob(job.ID, "user", "comment during pull")
			require.NoError(t, err)
		case 2:
			require.NoError(t, h.db.MarkReviewClosedByJobID(job.ID, true))
		}
		// Each call queues GetMachineID and its own write. Allow the pull
		// completing before either write and after the final release.
		require.LessOrEqual(t, commits.Load()-before, int64(4), "user write %d waited behind the pull", i)
	}
}

func TestReadTransactionDoesNotHoldWriters(t *testing.T) {
	t.Parallel()
	h := newSyncTestHelper(t)
	job := h.createCompletedJob("read-transaction")
	tx, err := h.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var count int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM reviews`).Scan(&count))
	done := make(chan error, 1)
	go func() {
		_, err := h.db.AddCommentToJob(job.ID, "user", "comment beside reader")
		done <- err
	}()
	// Wall-clock wait: the writer uses a separate real SQLite connection.
	timedOut := false
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second): //nolint:kennlint // deadlinetest: real SQLite reader and writer
		timedOut = true
		require.NoError(t, tx.Rollback())
		require.NoError(t, <-done)
	}
	assert.False(t, timedOut, "read transaction blocked the writer")
}

func TestPulledWritesWaitForWriter(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"job", "review"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			h := newSyncTestHelper(t)
			job := h.createPendingJob("pull-contention")
			setStatus(t, h.db, job.ID, JobStatusDone)
			now := time.Now().UTC()
			enqueuedAt := storedJobEnqueuedAt(t, h.db, job.ID)
			owner := storedJobSourceMachineID(t, h.db, job.ID)
			pull := func() error {
				return h.db.UpsertPulledJob(PulledJob{
					UUID: *job.UUID, GitRef: "updated-ref", Agent: "test",
					JobType: JobTypeReview, Status: string(JobStatusDone),
					EnqueuedAt: enqueuedAt, SourceMachineID: owner, UpdatedAt: now,
				}, h.repo.ID, nil)
			}
			if kind == "review" {
				pull = func() error {
					return h.db.UpsertPulledReview(PulledReview{
						UUID: testUUID("contended-review"), JobUUID: *job.UUID, Agent: "test",
						JobEnqueuedAt: enqueuedAt, JobSourceMachineID: owner,
						StructuredOutput:   reviewFixtureJSON("pulled finding"),
						UpdatedByMachineID: testUUID("peer"), CreatedAt: now, UpdatedAt: now,
					})
				}
			}

			writer, err := h.db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = writer.Rollback() }()
			_, err = writer.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('writer-lock', 'held')`)
			require.NoError(t, err)

			done := make(chan error, 1)
			go func() { done <- pull() }()
			// Wall-clock wait: hold a real SQLite writer while the pull starts.
			var pullErr error
			finished := false
			select {
			case pullErr = <-done:
				finished = true
			case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: holds a real SQLite writer while the pull waits for its lock
			}
			require.NoError(t, writer.Commit())
			if !finished {
				pullErr = <-done
			}
			require.NoError(t, pullErr)
			if kind == "job" {
				stored, err := h.db.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(t, "updated-ref", stored.GitRef)
			} else {
				stored, err := h.db.GetReviewByJobID(job.ID)
				require.NoError(t, err)
				assert.Equal(t, testUUID("contended-review"), *stored.UUID)
			}
		})
	}
}
