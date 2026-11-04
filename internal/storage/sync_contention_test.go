package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
