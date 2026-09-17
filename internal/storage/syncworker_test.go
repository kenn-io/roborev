package storage

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestSyncPullWritesNotifyAfterEachCommittedMutation(t *testing.T) {
	h := newSyncTestHelper(t)
	worker := NewSyncWorker(h.db, testSyncConfig())
	var notifications atomic.Int64
	worker.SetAfterPullWrite(func() { notifications.Add(1) })

	now := time.Now().UTC().Truncate(time.Second)
	finished := now
	job := PulledJob{
		UUID: testUUID("search-wake-job"), RepoIdentity: "github.com/example/search-wake",
		CommitSHA: "abc123", CommitAuthor: "Test", CommitSubject: "search wake",
		CommitTimestamp: now, GitRef: "abc123", Agent: "test", Reasoning: "thorough",
		JobType: JobTypeReview, ReviewType: "code", Status: string(JobStatusDone),
		EnqueuedAt: now, FinishedAt: &finished, SourceMachineID: testUUID("search-wake-machine"),
		UpdatedAt: now,
	}
	require.NoError(t, worker.pullJob(job))
	assert.Equal(t, int64(3), notifications.Load(), "repo, commit, and job each notify")

	review := PulledReview{
		UUID: testUUID("search-wake-review"), JobUUID: job.UUID, Agent: "test",
		Prompt: "prompt", StructuredOutput: reviewFixtureJSON("searchable finding"),
		UpdatedByMachineID: testUUID("search-wake-review-machine"), CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, worker.pullReview(review))
	assert.Equal(t, int64(4), notifications.Load())

	response := PulledResponse{
		UUID: testUUID("search-wake-response"), JobUUID: job.UUID,
		Responder: "human", Response: "fixed", SourceMachineID: testUUID("search-wake-response-machine"),
		CreatedAt: now,
	}
	require.NoError(t, worker.pullResponse(response))
	assert.Equal(t, int64(5), notifications.Load())

	// Replaying the cursor lookback window must not schedule redundant work.
	require.NoError(t, worker.pullJob(job))
	require.NoError(t, worker.pullReview(review))
	require.NoError(t, worker.pullResponse(response))
	assert.Equal(t, int64(5), notifications.Load())
}

func TestSyncPullWritesDoNotNotifyRejectedRolledBackOrOrphanRows(t *testing.T) {
	h := newSyncTestHelper(t)
	worker := NewSyncWorker(h.db, testSyncConfig())
	var notifications atomic.Int64
	worker.SetAfterPullWrite(func() { notifications.Add(1) })

	now := time.Now().UTC()
	missingJob := testUUID("missing-search-wake-job")
	require.NoError(t, worker.pullReview(PulledReview{
		UUID: testUUID("orphan-review"), JobUUID: missingJob, Agent: "test",
		UpdatedByMachineID: testUUID("orphan-review-machine"), CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, worker.pullResponse(PulledResponse{
		UUID: testUUID("orphan-response"), JobUUID: missingJob,
		Responder: "human", Response: "orphan", SourceMachineID: testUUID("orphan-response-machine"),
		CreatedAt: now,
	}))
	assert.Zero(t, notifications.Load())

	job := PulledJob{
		UUID: testUUID("invalid-review-parent"), RepoIdentity: "github.com/example/no-notify",
		GitRef: "main", Agent: "test", Reasoning: "thorough", JobType: JobTypeReview,
		ReviewType: "code", Status: string(JobStatusDone), EnqueuedAt: now,
		SourceMachineID: testUUID("invalid-review-parent-machine"), UpdatedAt: now,
	}
	require.NoError(t, worker.pullJob(job))
	before := notifications.Load()
	err := worker.pullReview(PulledReview{
		UUID: testUUID("invalid-review"), JobUUID: job.UUID, Agent: "test",
		StructuredOutput:   []byte(`{"schema_version":`),
		UpdatedByMachineID: testUUID("invalid-review-machine"), CreatedAt: now, UpdatedAt: now,
	})
	require.Error(t, err)
	assert.Equal(t, before, notifications.Load(), "failed review transaction must not notify")

	_, err = h.db.Exec(`UPDATE review_jobs SET status = 'applied' WHERE uuid = ?`, job.UUID)
	require.NoError(t, err)
	stale := PulledJob{
		UUID: job.UUID, RepoIdentity: job.RepoIdentity, GitRef: "stale", Agent: "test",
		Reasoning: "thorough", JobType: JobTypeReview, ReviewType: "code",
		Status: string(JobStatusDone), EnqueuedAt: now.Add(-time.Hour),
		SourceMachineID: job.SourceMachineID, UpdatedAt: now.Add(-time.Hour),
	}
	require.NoError(t, worker.pullJob(stale))
	assert.Equal(t, before, notifications.Load(), "rejected stale job must not notify")
}

func TestSyncPullReviewRollsBackIntermediateWriteWithoutNotification(t *testing.T) {
	h := newSyncTestHelper(t)
	worker := NewSyncWorker(h.db, testSyncConfig())
	var notifications atomic.Int64
	worker.SetAfterPullWrite(func() { notifications.Add(1) })

	now := time.Now().UTC().Truncate(time.Second)
	job := PulledJob{
		UUID: testUUID("rollback-review-parent"), RepoIdentity: "github.com/example/rollback-review",
		GitRef: "main", Agent: "test", Reasoning: "thorough", JobType: JobTypeReview,
		ReviewType: "code", Status: string(JobStatusDone), EnqueuedAt: now,
		SourceMachineID: testUUID("rollback-review-parent-machine"), UpdatedAt: now,
	}
	require.NoError(t, worker.pullJob(job))
	before := notifications.Load()

	var jobID int64
	require.NoError(t, h.db.QueryRow(`SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&jobID))
	reviewUUID := testUUID("rollback-review")
	_, err := h.db.Exec(`
		INSERT INTO legacy_reviews (
			job_id, agent, prompt, output, created_at, closed, uuid, migration_error
		) VALUES (?, 'test', 'prompt', 'legacy', ?, 0, ?, 'pending conversion')
	`, jobID, now.Format(time.RFC3339), reviewUUID)
	require.NoError(t, err)
	_, err = h.db.Exec(`
		CREATE TRIGGER fail_pulled_review_legacy_resolution
		BEFORE UPDATE OF resolved_at ON legacy_reviews
		BEGIN
			SELECT RAISE(ABORT, 'injected late review transaction failure');
		END
	`)
	require.NoError(t, err)

	err = worker.pullReview(PulledReview{
		UUID: reviewUUID, JobUUID: job.UUID, Agent: "test", Prompt: "prompt",
		StructuredOutput:   reviewFixtureJSON("transaction should roll back"),
		UpdatedByMachineID: testUUID("rollback-review-machine"), CreatedAt: now, UpdatedAt: now,
	})
	require.ErrorContains(t, err, "injected late review transaction failure")
	assert.Equal(t, before, notifications.Load(), "rolled-back review must not notify")

	var reviewCount int
	require.NoError(t, h.db.QueryRow(`SELECT count(*) FROM reviews WHERE uuid = ?`, reviewUUID).Scan(&reviewCount))
	assert.Zero(t, reviewCount, "review insert before the injected failure must roll back")
	var resolvedAt *string
	require.NoError(t, h.db.QueryRow(`SELECT resolved_at FROM legacy_reviews WHERE uuid = ?`, reviewUUID).Scan(&resolvedAt))
	assert.Nil(t, resolvedAt)
}

func TestSyncPullWriteCallbackIsRaceSafeNilSafeAndInvokedOutsideLock(t *testing.T) {
	h := newSyncTestHelper(t)
	worker := NewSyncWorker(h.db, testSyncConfig())
	worker.SetAfterPullWrite(nil)
	worker.notifyAfterPullWrite()

	done := make(chan struct{})
	worker.SetAfterPullWrite(func() {
		worker.SetAfterPullWrite(nil)
		close(done)
	})
	go worker.notifyAfterPullWrite()
	callbackCompleted := false
	select {
	case <-done:
		callbackCompleted = true
	case <-time.After(time.Second):
	}
	require.True(t, callbackCompleted, "callback ran while the worker mutex was held")

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			worker.SetAfterPullWrite(func() {})
		}()
		go func() {
			defer wg.Done()
			worker.notifyAfterPullWrite()
		}()
	}
	wg.Wait()
}

func testSyncConfig() config.SyncConfig {
	return config.SyncConfig{Enabled: true, Interval: "1h"}
}
