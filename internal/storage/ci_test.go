package storage

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

const (
	testRepo   = "myorg/myrepo"
	testSHA    = "sha1"
	testAgent  = "codex"
	testReview = "security"
)

func assertEq[T comparable](t *testing.T, name string, got, want T) {
	t.Helper()
	assert.Equalf(t, want, got, "assertion failed for %s: got=%v, want=%v", name, got, want)
}

func mustCreateCIBatch(t *testing.T, db *DB, ghRepo string, prNum int, headSHA string, totalJobs int) *CIPRBatch {
	t.Helper()
	batch, _, err := db.CreateCIBatch(ghRepo, prNum, headSHA, totalJobs)
	require.NoError(t, err, "CreateCIBatch: %v")

	return batch
}

func mustEnqueueReviewJob(t *testing.T, db *DB, repoID int64, gitRef, agent, reviewType string) *ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repoID, GitRef: gitRef, Agent: agent, ReviewType: reviewType,
	})
	require.NoError(t, err, "EnqueueJob: %v")

	return job
}

func mustRecordBatchJob(t *testing.T, db *DB, batchID, jobID int64) {
	t.Helper()
	if err := db.RecordBatchJob(batchID, jobID); err != nil {
		require.NoError(t, err, "RecordBatchJob: %v")
	}
}

func mustCreateLinkedBatchJob(t *testing.T, db *DB, repoID int64, ghRepo string, prNum int, headSHA, gitRef, agent, reviewType string) (*CIPRBatch, *ReviewJob) {
	t.Helper()
	batch := mustCreateCIBatch(t, db, ghRepo, prNum, headSHA, 1)
	job := mustEnqueueReviewJob(t, db, repoID, gitRef, agent, reviewType)
	mustRecordBatchJob(t, db, batch.ID, job.ID)
	return batch, job
}

func mustCreateLinkedTerminalJob(t *testing.T, db *DB, repoID int64, ghRepo string, prNum int, headSHA, gitRef, agent, reviewType, status string) (*CIPRBatch, int64) {
	t.Helper()
	batch, job := mustCreateLinkedBatchJob(t, db, repoID, ghRepo, prNum, headSHA, gitRef, agent, reviewType)
	setJobStatus(t, db, job.ID, JobStatus(status))
	return batch, job.ID
}

func setBatchTimestamp(t *testing.T, db *DB, batchID int64, column string, offset time.Duration) {
	t.Helper()
	ts := time.Now().UTC().Add(offset).Format("2006-01-02 15:04:05")
	query := `UPDATE ci_pr_batches SET ` + column + ` = ? WHERE id = ?`
	_, err := db.Exec(query, ts, batchID)
	require.NoError(t, err, "setBatchTimestamp (%s): %v", column, err)
}

func setBatchCreatedAt(t *testing.T, db *DB, batchID int64, offset time.Duration) {
	setBatchTimestamp(t, db, batchID, "created_at", offset)
}

func setBatchClaimedAt(t *testing.T, db *DB, batchID int64, offset time.Duration) {
	setBatchTimestamp(t, db, batchID, "claimed_at", offset)
}

func setJobStatusAndError(t *testing.T, db *DB, jobID int64, status, errorMsg string) {
	t.Helper()
	res, err := db.Exec(`UPDATE review_jobs SET status=?, error=? WHERE id=?`, status, errorMsg, jobID)
	require.NoError(t, err, "setJobStatusAndError: %v")

	rows, err := res.RowsAffected()
	require.NoError(t, err, "Failed to get rows affected: %v")

	if rows != 1 {
		require.Condition(t, func() bool { return false }, "Expected exactly 1 row updated for jobID %d, got %d", jobID, rows)
	}
}

func mustAddReview(t *testing.T, db *DB, jobID int64, agent, output string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output) VALUES (?, ?, 'test-prompt', ?)`, jobID, agent, output); err != nil {
		require.NoError(t, err, "mustAddReview: %v")
	}
}

func getBatch(t *testing.T, db *DB, id int64) *CIPRBatch {
	t.Helper()
	var b CIPRBatch
	var synthesized int
	err := db.QueryRow(`SELECT id, total_jobs, completed_jobs, failed_jobs, synthesized FROM ci_pr_batches WHERE id = ?`, id).Scan(
		&b.ID, &b.TotalJobs, &b.CompletedJobs, &b.FailedJobs, &synthesized,
	)
	require.NoError(t, err, "getBatch: %v")

	b.Synthesized = synthesized != 0
	return &b
}

func TestCreateCIBatch(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	batch, created, err := db.CreateCIBatch(testRepo, 42, "abc123", 4)
	require.NoError(t, err, "CreateCIBatch: %v")

	assertEq(t, "created", created, true)
	assert.NotZero(t, batch.ID, "expected non-zero batch ID")
	assertEq(t, "GithubRepo", batch.GithubRepo, testRepo)
	assertEq(t, "PRNumber", batch.PRNumber, 42)
	assertEq(t, "HeadSHA", batch.HeadSHA, "abc123")
	assertEq(t, "TotalJobs", batch.TotalJobs, 4)
	assertEq(t, "CompletedJobs", batch.CompletedJobs, 0)
	assertEq(t, "FailedJobs", batch.FailedJobs, 0)
	assertEq(t, "Synthesized", batch.Synthesized, false)

	batch2, created2, err := db.CreateCIBatch(testRepo, 42, "abc123", 4)
	require.NoError(t, err, "CreateCIBatch duplicate: %v")

	assertEq(t, "created", created2, false)
	assertEq(t, "ID", batch2.ID, batch.ID)
}

func TestHasCIBatch(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	t.Run("BeforeCreation", func(t *testing.T) {
		has, err := db.HasCIBatch(testRepo, 1, testSHA)
		require.NoError(t, err, "HasCIBatch: %v")

		assertEq(t, "has", has, false)
	})

	repo, err := db.GetOrCreateRepo("/tmp/test-repo-hasbatch")
	require.NoError(t, err, "GetOrCreateRepo: %v")

	batch := mustCreateCIBatch(t, db, testRepo, 1, testSHA, 2)

	t.Run("EmptyBatch", func(t *testing.T) {
		has, err := db.HasCIBatch(testRepo, 1, testSHA)
		require.NoError(t, err, "HasCIBatch (empty): %v")

		assertEq(t, "has", has, false)
	})

	t.Run("WithLinkedJob", func(t *testing.T) {
		job := mustEnqueueReviewJob(t, db, repo.ID, "abc..def", "test", testReview)
		mustRecordBatchJob(t, db, batch.ID, job.ID)

		has, err := db.HasCIBatch(testRepo, 1, testSHA)
		require.NoError(t, err, "HasCIBatch: %v")

		assertEq(t, "has", has, true)
	})

	t.Run("DifferentSHA", func(t *testing.T) {
		has, err := db.HasCIBatch(testRepo, 1, "sha2")
		require.NoError(t, err, "HasCIBatch: %v")

		assertEq(t, "has", has, false)
	})
}

func TestRecordBatchJob(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-repo")
	require.NoError(t, err, "GetOrCreateRepo: %v")

	batch := mustCreateCIBatch(t, db, testRepo, 1, testSHA, 2)
	job1 := mustEnqueueReviewJob(t, db, repo.ID, "abc..def", testAgent, testReview)
	job2 := mustEnqueueReviewJob(t, db, repo.ID, "abc..def", "gemini", "review")
	mustRecordBatchJob(t, db, batch.ID, job1.ID)
	mustRecordBatchJob(t, db, batch.ID, job2.ID)

	found, err := db.GetCIBatchByJobID(job1.ID)
	require.NoError(t, err, "GetCIBatchByJobID: %v")

	assert.NotNil(t, found, "expected batch by job ID")
	assert.Equalf(t, batch.ID, found.ID, "expected batch ID %d, got %v", batch.ID, found.ID)

	found2, err := db.GetCIBatchByJobID(job2.ID)
	require.NoError(t, err, "GetCIBatchByJobID: %v")

	assert.NotNil(t, found2, "expected batch by job ID")
	assert.Equalf(t, batch.ID, found2.ID, "expected batch ID %d, got %v", batch.ID, found2.ID)
}

func TestAttachJobAndBumpTotal_TerminalJobBumpsCounters(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-repo-attachbump")
	require.NoError(t, err)

	cases := []struct {
		status        JobStatus
		wantCompleted int
		wantFailed    int
	}{
		{JobStatusQueued, 0, 0},
		{JobStatusRunning, 0, 0},
		{JobStatusDone, 1, 0},
		{JobStatusSkipped, 1, 0},
		{JobStatusApplied, 1, 0},
		{JobStatusRebased, 1, 0},
		{JobStatusFailed, 0, 1},
		{JobStatusCanceled, 0, 1},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			batch := mustCreateCIBatch(t, db, testRepo, 1, "sha-"+string(tc.status), 0)
			job := mustEnqueueReviewJob(t, db, repo.ID, "ref-"+string(tc.status), testAgent, testReview)
			_, err := db.Exec(`UPDATE review_jobs SET status = ? WHERE id = ?`, string(tc.status), job.ID)
			require.NoError(t, err)

			total, err := db.AttachJobAndBumpTotal(batch.ID, job.ID)
			require.NoError(t, err)
			assert.Equal(t, 1, total, "total_jobs")

			got := getBatch(t, db, batch.ID)
			assert.Equal(t, 1, got.TotalJobs, "TotalJobs")
			assert.Equal(t, tc.wantCompleted, got.CompletedJobs, "CompletedJobs")
			assert.Equal(t, tc.wantFailed, got.FailedJobs, "FailedJobs")
		})
	}
}

func TestAttachJobAndBumpTotal_Idempotent(t *testing.T) {
	// A producer race must not double-bump the batch counters.
	// The unique index on (batch_id, job_id) + INSERT OR IGNORE
	// keeps the second attach a no-op so total_jobs (and any
	// terminal-status counter) only increment once.
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-repo-attach-idem")
	require.NoError(t, err)

	batch := mustCreateCIBatch(t, db, testRepo, 1, "sha-idem", 0)
	job := mustEnqueueReviewJob(t, db, repo.ID, "ref-idem", testAgent, testReview)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'done' WHERE id = ?`, job.ID)
	require.NoError(t, err)

	total1, err := db.AttachJobAndBumpTotal(batch.ID, job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, total1)

	total2, err := db.AttachJobAndBumpTotal(batch.ID, job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, total2, "duplicate attach must not bump total_jobs")

	got := getBatch(t, db, batch.ID)
	assert.Equal(t, 1, got.TotalJobs, "TotalJobs stays at 1")
	assert.Equal(t, 1, got.CompletedJobs, "CompletedJobs stays at 1")
	assert.Equal(t, 0, got.FailedJobs)

	var linkCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM ci_pr_batch_jobs WHERE batch_id=? AND job_id=?`,
		batch.ID, job.ID).Scan(&linkCount)
	require.NoError(t, err)
	assert.Equal(t, 1, linkCount, "only one link row exists for the (batch, job) pair")
}

func TestEnsureCIPRBatchJobsUniqueIndex_DedupesExistingRows(t *testing.T) {
	// Simulates an upgraded DB where an earlier version of
	// AttachJobAndBumpTotal inserted duplicate (batch_id, job_id)
	// rows before the unique index was introduced. The migration
	// must clean them up AND create the index so the idempotency
	// guarantee that INSERT OR IGNORE relies on actually holds.
	// It also has to repair counters on affected batches —
	// duplicate attaches inflated total_jobs/completed_jobs at the
	// time of insertion, and those must match the post-dedup row
	// count or synthesis stays stuck.
	db := openTestDB(t)
	defer db.Close()

	// Drop the index that the initial migration created, simulating
	// the pre-migration state.
	_, err := db.Exec(`DROP INDEX IF EXISTS idx_ci_pr_batch_jobs_uniq`)
	require.NoError(t, err)

	repo, err := db.GetOrCreateRepo("/tmp/test-repo-dedup-attach-mig")
	require.NoError(t, err)
	batch := mustCreateCIBatch(t, db, testRepo, 1, "sha-mig", 0)
	job := mustEnqueueReviewJob(t, db, repo.ID, "ref-mig", testAgent, testReview)
	_, err = db.Exec(`UPDATE review_jobs SET status='done' WHERE id=?`, job.ID)
	require.NoError(t, err)

	// Insert three duplicate link rows (could not happen with the
	// unique index in place, but our "upgraded DB" doesn't have it)
	// and artificially inflate the batch's counters to match what
	// the earlier buggy AttachJobAndBumpTotal would have left
	// behind: 3 attaches × (+1 total, +1 completed for a done job).
	for range 3 {
		_, err := db.Exec(`INSERT INTO ci_pr_batch_jobs (batch_id, job_id) VALUES (?, ?)`,
			batch.ID, job.ID)
		require.NoError(t, err)
	}
	_, err = db.Exec(`UPDATE ci_pr_batches
		SET total_jobs = 3, completed_jobs = 3 WHERE id = ?`, batch.ID)
	require.NoError(t, err)

	var before int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM ci_pr_batch_jobs
		WHERE batch_id = ? AND job_id = ?`, batch.ID, job.ID).Scan(&before))
	require.Equal(t, 3, before)

	require.NoError(t, db.ensureCIPRBatchJobsUniqueIndex())

	var after int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM ci_pr_batch_jobs
		WHERE batch_id = ? AND job_id = ?`, batch.ID, job.ID).Scan(&after))
	assert.Equal(t, 1, after, "duplicates must be collapsed to a single link row")

	got := getBatch(t, db, batch.ID)
	assert.Equal(t, 1, got.TotalJobs,
		"total_jobs must match remaining link count, not pre-dedup inflated value")
	assert.Equal(t, 1, got.CompletedJobs,
		"completed_jobs must be recalculated from the remaining done job")
	assert.Equal(t, 0, got.FailedJobs)

	// With the unique index in place, a further attempt to re-insert
	// the duplicate is rejected, so INSERT OR IGNORE makes
	// AttachJobAndBumpTotal truly idempotent.
	res, err := db.Exec(`INSERT OR IGNORE INTO ci_pr_batch_jobs (batch_id, job_id) VALUES (?, ?)`,
		batch.ID, job.ID)
	require.NoError(t, err)
	inserted, err := res.RowsAffected()
	require.NoError(t, err)
	assert.EqualValues(t, 0, inserted,
		"unique index must make re-insert a no-op")
}

func TestEnsureCIPRBatchJobsUniqueIndex_RepairsFailedCount(t *testing.T) {
	// Companion to *_DedupesExistingRows — covers the failed_jobs
	// CASE branch in the counter-repair UPDATE so a future refactor
	// of the failed/canceled status list can't silently regress
	// upgraded databases with duplicated failed jobs.
	db := openTestDB(t)
	defer db.Close()

	_, err := db.Exec(`DROP INDEX IF EXISTS idx_ci_pr_batch_jobs_uniq`)
	require.NoError(t, err)

	repo, err := db.GetOrCreateRepo("/tmp/test-repo-dedup-failed")
	require.NoError(t, err)
	batch := mustCreateCIBatch(t, db, testRepo, 2, "sha-failed-mig", 0)
	job := mustEnqueueReviewJob(t, db, repo.ID, "ref-failed-mig", testAgent, testReview)
	_, err = db.Exec(`UPDATE review_jobs SET status='failed' WHERE id=?`, job.ID)
	require.NoError(t, err)

	for range 3 {
		_, err := db.Exec(`INSERT INTO ci_pr_batch_jobs (batch_id, job_id) VALUES (?, ?)`,
			batch.ID, job.ID)
		require.NoError(t, err)
	}
	_, err = db.Exec(`UPDATE ci_pr_batches SET total_jobs = 3, failed_jobs = 3 WHERE id = ?`, batch.ID)
	require.NoError(t, err)

	require.NoError(t, db.ensureCIPRBatchJobsUniqueIndex())

	got := getBatch(t, db, batch.ID)
	assert.Equal(t, 1, got.TotalJobs)
	assert.Equal(t, 0, got.CompletedJobs)
	assert.Equal(t, 1, got.FailedJobs,
		"failed_jobs must be recalculated from remaining failed links")
}

func TestIncrementBatchCompleted(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	batch, _, err := db.CreateCIBatch(testRepo, 1, testSHA, 3)
	require.NoError(t, err, "CreateCIBatch: %v")

	updated, err := db.IncrementBatchCompleted(batch.ID)
	require.NoError(t, err, "IncrementBatchCompleted: %v")
	assert.Equal(t, 1, updated.CompletedJobs, "got CompletedJobs=%d, want 1", updated.CompletedJobs)

	updated, err = db.IncrementBatchCompleted(batch.ID)
	require.NoError(t, err, "IncrementBatchCompleted: %v")
	assert.Equal(t, 2, updated.CompletedJobs, "got CompletedJobs=%d, want 2", updated.CompletedJobs)

}

func TestIncrementBatchFailed(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	batch, _, err := db.CreateCIBatch(testRepo, 1, testSHA, 3)
	require.NoError(t, err, "CreateCIBatch: %v")

	updated, err := db.IncrementBatchFailed(batch.ID)
	require.NoError(t, err, "IncrementBatchFailed: %v")
	assert.Equal(t, 1, updated.FailedJobs, "got FailedJobs=%d, want 1", updated.FailedJobs)

	updated, err = db.IncrementBatchCompleted(batch.ID)
	require.NoError(t, err, "IncrementBatchCompleted: %v")

	assert.Equal(t, 1, updated.CompletedJobs, "got CompletedJobs=%d, FailedJobs=%d, want 1, 1", updated.CompletedJobs, updated.FailedJobs)
	assert.Equal(t, 1, updated.FailedJobs, "got CompletedJobs=%d, FailedJobs=%d, want 1, 1", updated.CompletedJobs, updated.FailedJobs)
}

func TestIncrementBatchConcurrent(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	n := 10
	batch, _, err := db.CreateCIBatch(testRepo, 1, testSHA, n)
	require.NoError(t, err, "CreateCIBatch: %v")

	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			_, err := db.IncrementBatchCompleted(batch.ID)
			assert.NoError(t, err, "IncrementBatchCompleted")
		})
	}
	wg.Wait()

	finalBatch := getBatch(t, db, batch.ID)
	assert.Equal(t, n, finalBatch.CompletedJobs, "got CompletedJobs=%d, want %d", finalBatch.CompletedJobs, n)

}

func TestGetBatchReviews(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-repo")
	require.NoError(t, err, "GetOrCreateRepo: %v")

	batch := mustCreateCIBatch(t, db, testRepo, 1, testSHA, 2)
	job1 := mustEnqueueReviewJob(t, db, repo.ID, "abc..def", testAgent, testReview)
	job2 := mustEnqueueReviewJob(t, db, repo.ID, "abc..def", "gemini", "review")
	mustRecordBatchJob(t, db, batch.ID, job1.ID)
	mustRecordBatchJob(t, db, batch.ID, job2.ID)

	setJobStatus(t, db, job1.ID, JobStatusDone)
	mustAddReview(t, db, job1.ID, testAgent, "finding1")

	setJobStatusAndError(t, db, job2.ID, "failed", "timeout")

	reviews, err := db.GetBatchReviews(batch.ID)
	require.NoError(t, err, "GetBatchReviews: %v")

	if len(reviews) != 2 {
		assert.Len(t, reviews, 2, "got %d reviews, want 2", len(reviews))
	}

	assertEq(t, "review[0].Agent", reviews[0].Agent, testAgent)
	assertEq(t, "review[0].ReviewType", reviews[0].ReviewType, testReview)
	assertEq(t, "review[0].Output", reviews[0].Output, "finding1")
	assertEq(t, "review[0].Status", reviews[0].Status, "done")

	assertEq(t, "review[1].Agent", reviews[1].Agent, "gemini")
	assertEq(t, "review[1].ReviewType", reviews[1].ReviewType, "review")
	assertEq(t, "review[1].Status", reviews[1].Status, "failed")
	assertEq(t, "review[1].Error", reviews[1].Error, "timeout")
}

func TestGetCIBatchByJobID(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	batch, job := mustCreateLinkedBatchJob(t, db, repo.ID, testRepo, 1, testSHA, "abc..def", testAgent, testReview)

	found, err := db.GetCIBatchByJobID(job.ID)
	require.NoError(t, err, "GetCIBatchByJobID: %v")
	require.NotNil(t, found, "expected non-nil batch")

	assert.Equal(t, batch.ID, found.ID, "got batch ID %d, want %d", found.ID, batch.ID)

	notFound, err := db.GetCIBatchByJobID(99999)
	require.NoError(t, err, "GetCIBatchByJobID: %v")
	assert.Nil(t, notFound, "expected nil for unknown job ID")

}

func TestListCIBatchesByJobID_Multiple(t *testing.T) {
	// A shared auto-design row links to every batch that dedup'd onto it.
	// The completion event handler must advance every linked batch; if
	// ListCIBatchesByJobID returned only one, later batches would stay
	// short on completed_jobs until stale reconciliation.
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-repo-shared-job")
	require.NoError(t, err)

	batchA := mustCreateCIBatch(t, db, testRepo, 1, testSHA, 1)
	batchB := mustCreateCIBatch(t, db, testRepo, 2, testSHA, 1)
	shared := mustEnqueueReviewJob(t, db, repo.ID, testSHA, testAgent, testReview)
	mustRecordBatchJob(t, db, batchA.ID, shared.ID)
	mustRecordBatchJob(t, db, batchB.ID, shared.ID)

	got, err := db.ListCIBatchesByJobID(shared.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)

	ids := []int64{got[0].ID, got[1].ID}
	assert.Contains(t, ids, batchA.ID)
	assert.Contains(t, ids, batchB.ID)

	empty, err := db.ListCIBatchesByJobID(99999)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestClaimBatchForSynthesis(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	batch, _, _ := db.CreateCIBatch(testRepo, 1, testSHA, 1)
	assert.False(t, batch.Synthesized, "expected Synthesized=false initially")

	claimed, err := db.ClaimBatchForSynthesis(batch.ID)
	require.NoError(t, err, "ClaimBatchForSynthesis: %v")

	assert.True(t, claimed, "expected first claim to succeed")

	claimed, err = db.ClaimBatchForSynthesis(batch.ID)
	require.NoError(t, err, "ClaimBatchForSynthesis (second): %v")

	assert.False(t, claimed, "expected second claim to fail")

	if err := db.UnclaimBatch(batch.ID); err != nil {
		require.NoError(t, err, "UnclaimBatch: %v")
	}
	claimed, err = db.ClaimBatchForSynthesis(batch.ID)
	require.NoError(t, err, "ClaimBatchForSynthesis (after unclaim): %v")

	assert.True(t, claimed, "expected claim after unclaim to succeed")

}

func TestFinalizeBatch_PreventsStaleRepost(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test")
	require.NoError(t, err, "GetOrCreateRepo: %v")

	batch, _ := mustCreateLinkedTerminalJob(t, db, repo.ID, testRepo, 1, testSHA, testSHA, testAgent, testReview, "done")

	claimed, err := db.ClaimBatchForSynthesis(batch.ID)
	require.NoError(t, err)
	require.True(t, claimed, "expected claim to succeed")

	var claimedBefore sql.NullString
	if err := db.QueryRow(`SELECT claimed_at FROM ci_pr_batches WHERE id = ?`, batch.ID).Scan(&claimedBefore); err != nil {
		require.NoError(t, err, "scan claimed_at before finalize: %v")
	}
	require.True(t, claimedBefore.Valid, "expected claimed_at to be set after claim")

	if err := db.FinalizeBatch(batch.ID); err != nil {
		require.NoError(t, err, "FinalizeBatch: %v")
	}

	var claimedAfter sql.NullString
	if err := db.QueryRow(`SELECT claimed_at FROM ci_pr_batches WHERE id = ?`, batch.ID).Scan(&claimedAfter); err != nil {
		require.NoError(t, err, "scan claimed_at after finalize: %v")
	}
	assert.False(t, claimedAfter.Valid, "expected claimed_at to be NULL after finalize, got %q", claimedAfter.String)

	var synthesized int
	if err := db.QueryRow(`SELECT synthesized FROM ci_pr_batches WHERE id = ?`, batch.ID).Scan(&synthesized); err != nil {
		require.NoError(t, err, "scan synthesized after finalize: %v")
	}
	require.Equal(t, 1, synthesized, "expected synthesized=1 after finalize, got %d", synthesized)

	stale, err := db.GetStaleBatches()
	require.NoError(t, err, "GetStaleBatches: %v")

	for _, b := range stale {
		if b.ID == batch.ID {
			assert.NotEqual(t, batch.ID, b.ID, "finalized batch should not appear in stale batches")
		}
	}

	claimed, err = db.ClaimBatchForSynthesis(batch.ID)
	require.NoError(t, err)
	assert.False(t, claimed, "should not be able to re-claim a finalized batch")

}

func TestGetStaleBatches_StaleClaim(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test")
	require.NoError(t, err, "GetOrCreateRepo: %v")

	batch, _ := mustCreateLinkedTerminalJob(t, db, repo.ID, testRepo, 1, testSHA, testSHA, testAgent, testReview, "done")

	_, _ = db.ClaimBatchForSynthesis(batch.ID)
	setBatchClaimedAt(t, db, batch.ID, -10*time.Minute)

	stale, err := db.GetStaleBatches()
	require.NoError(t, err, "GetStaleBatches: %v")

	found := false
	for _, b := range stale {
		if b.ID == batch.ID {
			found = true
		}
	}
	assert.True(t, found, "stale claimed batch should appear in GetStaleBatches")

}

func TestUnclaimStaleBatchClaim_OnlyUnclaimsStillStaleClaims(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test")
	require.NoError(t, err, "GetOrCreateRepo: %v")

	batch, _ := mustCreateLinkedTerminalJob(t, db, repo.ID, testRepo, 1, testSHA, testSHA, testAgent, testReview, "done")

	claimed, err := db.ClaimBatchForSynthesis(batch.ID)
	require.NoError(t, err)
	require.True(t, claimed, "expected claim to succeed")

	setBatchClaimedAt(t, db, batch.ID, -10*time.Minute)
	unclaimed, err := db.UnclaimStaleBatchClaim(batch.ID, 5*time.Minute)
	require.NoError(t, err)
	assert.True(t, unclaimed, "stale claim should be unclaimed")

	claimed, err = db.ClaimBatchForSynthesis(batch.ID)
	require.NoError(t, err)
	require.True(t, claimed, "expected claim after stale unclaim to succeed")

	require.NoError(t, db.FinalizeBatch(batch.ID))

	unclaimed, err = db.UnclaimStaleBatchClaim(batch.ID, 5*time.Minute)
	require.NoError(t, err)
	assert.False(t, unclaimed, "finalized batch must not be unclaimed")

	claimed, err = db.ClaimBatchForSynthesis(batch.ID)
	require.NoError(t, err)
	assert.False(t, claimed, "finalized batch must not be claimable")
}

func TestDeleteEmptyBatches(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	emptyOld := mustCreateCIBatch(t, db, testRepo, 1, "sha-old", 2)
	setBatchCreatedAt(t, db, emptyOld.ID, -5*time.Minute)

	mustCreateCIBatch(t, db, testRepo, 2, "sha-recent", 1)

	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err, "GetOrCreateRepo: %v")

	nonEmpty, _ := mustCreateLinkedBatchJob(t, db, repo.ID, testRepo, 3, "sha-nonempty", "a..b", testAgent, testReview)
	setBatchCreatedAt(t, db, nonEmpty.ID, -5*time.Minute)

	n, err := db.DeleteEmptyBatches()
	require.NoError(t, err, "DeleteEmptyBatches: %v")

	assertEq(t, "deleted count", n, 1)

	has, err := db.HasCIBatch(testRepo, 1, "sha-old")
	require.NoError(t, err, "HasCIBatch (old empty): %v")

	assertEq(t, "has", has, false)

	var recentCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ci_pr_batches WHERE github_repo = ? AND pr_number = ? AND head_sha = ?`,
		testRepo, 2, "sha-recent").Scan(&recentCount); err != nil {
		require.NoError(t, err, "count recent batch: %v")
	}
	assertEq(t, "recentCount", recentCount, 1)

	has, err = db.HasCIBatch(testRepo, 3, "sha-nonempty")
	require.NoError(t, err, "HasCIBatch (non-empty): %v")

	assertEq(t, "has", has, true)
}

func TestCancelJob_ReturnsErrNoRowsForTerminalJobs(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-cancel")
	require.NoError(t, err, "GetOrCreateRepo: %v")

	t.Run("TerminalJob_ReturnsErrNoRows", func(t *testing.T) {
		job := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
		setJobStatus(t, db, job.ID, JobStatusDone)

		err := db.CancelJob(job.ID)
		require.ErrorIs(t, err, sql.ErrNoRows, "expected sql.ErrNoRows, got: %v", err)

	})

	t.Run("QueuedJob_Succeeds", func(t *testing.T) {
		job := mustEnqueueReviewJob(t, db, repo.ID, "c..d", testAgent, testReview)
		if err := db.CancelJob(job.ID); err != nil {
			require.NoError(t, err, "CancelJob on queued job: %v")
		}

		var status string
		if err := db.QueryRow(`SELECT status FROM review_jobs WHERE id = ?`, job.ID).Scan(&status); err != nil {
			require.NoError(t, err, "query status: %v")
		}
		assertEq(t, "status", status, "canceled")
	})
}

func TestCancelSupersededBatches(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-supersede")
	require.NoError(t, err, "GetOrCreateRepo: %v")

	oldBatch := mustCreateCIBatch(t, db, "owner/repo", 1, "oldsha", 2)
	job1 := mustEnqueueReviewJob(t, db, repo.ID, "base..oldsha", testAgent, testReview)
	job2 := mustEnqueueReviewJob(t, db, repo.ID, "base..oldsha", testAgent, "review")
	if err := db.RecordBatchJob(oldBatch.ID, job1.ID); err != nil {
		require.NoError(t, err, "RecordBatchJob: %v")
	}
	if err := db.RecordBatchJob(oldBatch.ID, job2.ID); err != nil {
		require.NoError(t, err, "RecordBatchJob: %v")
	}

	doneBatch := mustCreateCIBatch(t, db, "owner/repo", 1, "donesha", 1)
	doneJob := mustEnqueueReviewJob(t, db, repo.ID, "base..donesha", testAgent, testReview)
	if err := db.RecordBatchJob(doneBatch.ID, doneJob.ID); err != nil {
		require.NoError(t, err, "RecordBatchJob: %v")
	}
	if _, err := db.ClaimBatchForSynthesis(doneBatch.ID); err != nil {
		require.NoError(t, err, "ClaimBatchForSynthesis: %v")
	}

	canceledIDs, err := db.CancelSupersededBatches("owner/repo", 1, "newsha")
	require.NoError(t, err, "CancelSupersededBatches: %v")

	if len(canceledIDs) != 2 {
		assert.Len(t, canceledIDs, 2, "len(canceledIDs) = %d, want 2", len(canceledIDs))
	}

	has, err := db.HasCIBatch("owner/repo", 1, "oldsha")
	require.NoError(t, err, "HasCIBatch: %v")

	assert.False(t, has, "old batch should have been deleted")

	var status string
	if err := db.QueryRow(`SELECT status FROM review_jobs WHERE id = ?`, job1.ID).Scan(&status); err != nil {
		require.NoError(t, err, "query status: %v")
	}
	assert.Equal(t, "canceled", status, "job1 status = %q, want canceled", status)

	has, err = db.HasCIBatch("owner/repo", 1, "donesha")
	require.NoError(t, err, "HasCIBatch done: %v")

	assert.True(t, has, "synthesized batch should NOT have been canceled")

	canceledIDs, err = db.CancelSupersededBatches("owner/repo", 1, "newsha")
	require.NoError(t, err, "CancelSupersededBatches no-op: %v")

	if len(canceledIDs) != 0 {
		assert.Empty(t, canceledIDs, "expected 0 canceled on no-op, got %d", len(canceledIDs))
	}
}

func TestLatestBatchTimeForPR(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	t.Run("no batches returns zero time", func(t *testing.T) {
		ts, err := db.LatestBatchTimeForPR(testRepo, 99)
		require.NoError(t, err, "LatestBatchTimeForPR: %v")
		assert.True(t, ts.IsZero(), "expected zero time, got %v", ts)

	})

	t.Run("returns latest batch time", func(t *testing.T) {
		before := time.Now().UTC().Truncate(time.Second)

		repo, err := db.GetOrCreateRepo("/tmp/test-throttle")
		require.NoError(t, err, "GetOrCreateRepo: %v")

		batchA := mustCreateCIBatch(
			t, db, testRepo, 42, "sha-a", 1,
		)
		jobA := mustEnqueueReviewJob(
			t, db, repo.ID, "a..b", "codex", "security",
		)
		mustRecordBatchJob(t, db, batchA.ID, jobA.ID)

		batchB := mustCreateCIBatch(
			t, db, testRepo, 42, "sha-b", 1,
		)
		jobB := mustEnqueueReviewJob(
			t, db, repo.ID, "c..d", "codex", "security",
		)
		mustRecordBatchJob(t, db, batchB.ID, jobB.ID)

		ts, err := db.LatestBatchTimeForPR(testRepo, 42)
		require.NoError(t, err, "LatestBatchTimeForPR: %v")

		if ts.IsZero() {
			require.False(t, ts.IsZero(), "expected non-zero time")
		}

		if ts.Before(before.Add(-1 * time.Second)) {
			assert.False(t, ts.Before(before.Add(-1*time.Second)), "expected time >= %v, got %v", before, ts)
		}
	})

	t.Run("different PR returns zero", func(t *testing.T) {
		ts, err := db.LatestBatchTimeForPR(testRepo, 999)
		require.NoError(t, err, "LatestBatchTimeForPR: %v")
		assert.True(t, ts.IsZero(), "expected zero time for different PR, got %v", ts)

	})
}

func TestGetPendingBatchPRs(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-pending")
	require.NoError(t, err, "GetOrCreateRepo: %v")

	batch5 := mustCreateCIBatch(t, db, testRepo, 5, "sha5", 1)
	job5 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	mustRecordBatchJob(t, db, batch5.ID, job5.ID)

	batch7 := mustCreateCIBatch(t, db, testRepo, 7, "sha7", 1)
	job7 := mustEnqueueReviewJob(t, db, repo.ID, "c..d", testAgent, testReview)
	mustRecordBatchJob(t, db, batch7.ID, job7.ID)

	batch9 := mustCreateCIBatch(t, db, testRepo, 9, "sha9", 1)
	job9 := mustEnqueueReviewJob(t, db, repo.ID, "e..f", testAgent, testReview)
	mustRecordBatchJob(t, db, batch9.ID, job9.ID)
	if _, err := db.ClaimBatchForSynthesis(batch9.ID); err != nil {
		require.NoError(t, err, "ClaimBatchForSynthesis: %v")
	}

	batchOther := mustCreateCIBatch(t, db, "other/repo", 5, "sha-other", 1)
	jobOther := mustEnqueueReviewJob(t, db, repo.ID, "g..h", testAgent, testReview)
	mustRecordBatchJob(t, db, batchOther.ID, jobOther.ID)

	refs, err := db.GetPendingBatchPRs(testRepo)
	require.NoError(t, err, "GetPendingBatchPRs: %v")

	prNums := make(map[int]bool)
	for _, r := range refs {
		prNums[r.PRNumber] = true
		assertEq(t, "GithubRepo", r.GithubRepo, testRepo)
	}
	if !prNums[5] || !prNums[7] {
		assert.True(t, prNums[5] && prNums[7], "expected PRs 5 and 7, got %v", prNums)
	}
	assert.False(t, prNums[9], "synthesized PR #9 should not appear")

	if len(refs) != 2 {
		assert.Len(t, refs, 2, "expected 2 refs, got %d", len(refs))
	}
}

func TestCancelClosedPRBatches(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-cancel-closed")
	require.NoError(t, err, "GetOrCreateRepo: %v")

	batch := mustCreateCIBatch(t, db, testRepo, 5, "sha5", 2)
	job1 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	job2 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", "gemini", "review")
	mustRecordBatchJob(t, db, batch.ID, job1.ID)
	mustRecordBatchJob(t, db, batch.ID, job2.ID)

	doneBatch := mustCreateCIBatch(t, db, testRepo, 5, "sha5-done", 1)
	doneJob := mustEnqueueReviewJob(t, db, repo.ID, "c..d", testAgent, testReview)
	mustRecordBatchJob(t, db, doneBatch.ID, doneJob.ID)
	if _, err := db.ClaimBatchForSynthesis(doneBatch.ID); err != nil {
		require.NoError(t, err, "ClaimBatchForSynthesis: %v")
	}

	canceledIDs, err := db.CancelClosedPRBatches(testRepo, 5)
	require.NoError(t, err, "CancelClosedPRBatches: %v")

	if len(canceledIDs) != 2 {
		assert.Len(t, canceledIDs, 2, "expected 2 canceled jobs, got %d", len(canceledIDs))
	}

	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM ci_pr_batches WHERE id = ?`,
		batch.ID,
	).Scan(&count); err != nil {
		require.NoError(t, err, "count deleted batch: %v")
	}
	assertEq(t, "deleted batch count", count, 0)

	var status string
	if err := db.QueryRow(
		`SELECT status FROM review_jobs WHERE id = ?`, job1.ID,
	).Scan(&status); err != nil {
		require.NoError(t, err, "query job1 status: %v")
	}
	assertEq(t, "job1 status", status, "canceled")

	var doneCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM ci_pr_batches WHERE id = ?`,
		doneBatch.ID,
	).Scan(&doneCount); err != nil {
		require.NoError(t, err, "count done batch: %v")
	}
	assertEq(t, "done batch count", doneCount, 1)

	canceledIDs, err = db.CancelClosedPRBatches(testRepo, 5)
	require.NoError(t, err, "CancelClosedPRBatches no-op: %v")

	assert.Empty(t, canceledIDs)
}

func TestCancelClosedPRBatches_SkipsClaimedBatch(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-skip-claimed")
	require.NoError(t, err, "GetOrCreateRepo: %v")

	batch := mustCreateCIBatch(t, db, testRepo, 15, "sha15", 1)
	job := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	mustRecordBatchJob(t, db, batch.ID, job.ID)

	if _, err := db.Exec(
		`UPDATE review_jobs SET status='done' WHERE id = ?`, job.ID,
	); err != nil {
		require.NoError(t, err, "mark done: %v")
	}

	claimed, err := db.ClaimBatchForSynthesis(batch.ID)
	require.NoError(t, err, "ClaimBatchForSynthesis: %v")

	require.True(t, claimed, "expected successful claim")

	if _, err := db.Exec(
		`UPDATE ci_pr_batches SET synthesized = 0, claimed_at = datetime('now') WHERE id = ?`,
		batch.ID,
	); err != nil {
		require.NoError(t, err, "set claimed state: %v")
	}

	canceledIDs, err := db.CancelClosedPRBatches(testRepo, 15)
	require.NoError(t, err, "CancelClosedPRBatches: %v")

	assert.Empty(t, canceledIDs)

	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM ci_pr_batches WHERE id = ?`,
		batch.ID,
	).Scan(&count); err != nil {
		require.NoError(t, err, "count batch: %v")
	}
	assertEq(t, "claimed batch should survive", count, 1)
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

func TestGetNonTerminalBatchJobIDs(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-nonterminal")
	require.NoError(t, err)

	batch, _, err := db.CreateCIBatch("acme/api", 1, "abc123", 3)
	require.NoError(t, err)

	j1 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	j2 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	j3 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	for _, jid := range []int64{j1.ID, j2.ID, j3.ID} {
		require.NoError(t, db.RecordBatchJob(batch.ID, jid))
	}

	setJobStatus(t, db, j1.ID, JobStatusDone)
	setJobStatus(t, db, j2.ID, JobStatusFailed)

	ids, err := db.GetNonTerminalBatchJobIDs(batch.ID)
	require.NoError(t, err)
	assert.Equal(t, []int64{j3.ID}, ids)
}

func TestIsBatchExpired(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	batch, _, err := db.CreateCIBatch("acme/api", 1, "abc123", 2)
	require.NoError(t, err)

	t.Run("fresh batch is not expired", func(t *testing.T) {
		expired, err := db.IsBatchExpired(batch.ID, 3*time.Minute)
		require.NoError(t, err)
		assert.False(t, expired)
	})

	t.Run("old batch is expired", func(t *testing.T) {
		_, err := db.Exec(
			`UPDATE ci_pr_batches SET created_at = datetime('now', '-10 minutes') WHERE id = ?`,
			batch.ID)
		require.NoError(t, err)

		expired, err := db.IsBatchExpired(batch.ID, 3*time.Minute)
		require.NoError(t, err)
		assert.True(t, expired)
	})

	t.Run("sub-second timeout floors to 1s", func(t *testing.T) {
		// Reset to fresh
		_, err := db.Exec(
			`UPDATE ci_pr_batches SET created_at = CURRENT_TIMESTAMP WHERE id = ?`,
			batch.ID)
		require.NoError(t, err)

		// 500ms should floor to 1s, so a fresh batch is not expired
		expired, err := db.IsBatchExpired(batch.ID, 500*time.Millisecond)
		require.NoError(t, err)
		assert.False(t, expired, "sub-second timeout should floor to 1s, not 0")
	})
}

func TestGetExpiredBatches(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, err := db.GetOrCreateRepo("/tmp/test-expired")
	require.NoError(t, err)

	// Batch 1: old, has a completed job and a non-terminal job → returned
	b1, _, err := db.CreateCIBatch("acme/api", 1, "sha1", 2)
	require.NoError(t, err)
	j1 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	j2 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	require.NoError(t, db.RecordBatchJob(b1.ID, j1.ID))
	require.NoError(t, db.RecordBatchJob(b1.ID, j2.ID))
	setJobStatus(t, db, j1.ID, JobStatusDone)
	_, err = db.Exec(
		`UPDATE ci_pr_batches SET created_at = datetime('now', '-10 minutes') WHERE id = ?`,
		b1.ID)
	require.NoError(t, err)

	// Batch 2: old but all jobs terminal → NOT returned
	b2, _, err := db.CreateCIBatch("acme/api", 2, "sha2", 1)
	require.NoError(t, err)
	j3 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	require.NoError(t, db.RecordBatchJob(b2.ID, j3.ID))
	setJobStatus(t, db, j3.ID, JobStatusDone)
	_, err = db.Exec(
		`UPDATE ci_pr_batches SET created_at = datetime('now', '-10 minutes') WHERE id = ?`,
		b2.ID)
	require.NoError(t, err)

	// Batch 3: old, has non-terminal jobs but NO completed jobs → NOT returned
	b3, _, err := db.CreateCIBatch("acme/api", 3, "sha3", 2)
	require.NoError(t, err)
	j4 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	j5 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	require.NoError(t, db.RecordBatchJob(b3.ID, j4.ID))
	require.NoError(t, db.RecordBatchJob(b3.ID, j5.ID))
	_, err = db.Exec(
		`UPDATE ci_pr_batches SET created_at = datetime('now', '-10 minutes') WHERE id = ?`,
		b3.ID)
	require.NoError(t, err)

	// Batch 4: fresh with mixed status → NOT returned (not expired)
	b4, _, err := db.CreateCIBatch("acme/api", 4, "sha4", 2)
	require.NoError(t, err)
	j6 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	j7 := mustEnqueueReviewJob(t, db, repo.ID, "a..b", testAgent, testReview)
	require.NoError(t, db.RecordBatchJob(b4.ID, j6.ID))
	require.NoError(t, db.RecordBatchJob(b4.ID, j7.ID))
	setJobStatus(t, db, j6.ID, JobStatusDone)

	batches, err := db.GetExpiredBatches(3 * time.Minute)
	require.NoError(t, err)
	require.Len(t, batches, 1)
	assert.Equal(t, b1.ID, batches[0].ID)

	_ = b2
	_ = b3
	_ = b4
}

func TestGetStaleBatches_TreatsSkippedAsTerminal(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repoID := createRepo(t, db, "/tmp/repo-skipped-batch").ID
	commitID := createCommit(t, db, repoID, "abc123").ID
	var jobID int64
	err := db.QueryRow(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type)
		VALUES (?, ?, 'abc123', 'skipped', 'design') RETURNING id
	`, repoID, commitID).Scan(&jobID)
	require.NoError(t, err)

	var batchID int64
	err = db.QueryRow(`
		INSERT INTO ci_pr_batches (github_repo, pr_number, head_sha, total_jobs, completed_jobs)
		VALUES ('x/y', 1, 'abc123', 1, 0) RETURNING id
	`).Scan(&batchID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO ci_pr_batch_jobs (batch_id, job_id) VALUES (?, ?)`, batchID, jobID)
	require.NoError(t, err)

	batches, err := db.GetStaleBatches()
	require.NoError(t, err)

	found := false
	for _, b := range batches {
		if b.ID == batchID {
			found = true
		}
	}
	assert.True(t, found, "skipped-only batch must be considered terminal")
}

func TestReconcileBatch_CountsSkippedAsCompleted(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repoID := createRepo(t, db, "/tmp/repo-recon").ID
	commitID := createCommit(t, db, repoID, "cafef00d").ID
	var jobID int64
	err := db.QueryRow(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type)
		VALUES (?, ?, 'cafef00d', 'skipped', 'design') RETURNING id
	`, repoID, commitID).Scan(&jobID)
	require.NoError(t, err)

	var batchID int64
	err = db.QueryRow(`
		INSERT INTO ci_pr_batches (github_repo, pr_number, head_sha, total_jobs)
		VALUES ('x/y', 2, 'cafef00d', 1) RETURNING id
	`).Scan(&batchID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO ci_pr_batch_jobs (batch_id, job_id) VALUES (?, ?)`, batchID, jobID)
	require.NoError(t, err)

	batch, err := db.ReconcileBatch(batchID)
	require.NoError(t, err)
	assert.Equal(t, 1, batch.CompletedJobs)
	assert.Equal(t, 0, batch.FailedJobs)
}

func TestReconcileBatch_CountsAppliedAndRebasedAsCompleted(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// AttachJobAndBumpTotal and ReconcileBatch must classify the same
	// statuses the same way — otherwise reconciliation would subtract
	// applied/rebased jobs back out of completed_jobs after a correct
	// attach-time bump.
	repoID := createRepo(t, db, "/tmp/repo-recon-fix").ID
	commitID := createCommit(t, db, repoID, "f1xeddeed").ID

	var batchID int64
	err := db.QueryRow(`
		INSERT INTO ci_pr_batches (github_repo, pr_number, head_sha, total_jobs)
		VALUES ('x/y', 3, 'f1xeddeed', 2) RETURNING id
	`).Scan(&batchID)
	require.NoError(t, err)

	for _, status := range []JobStatus{JobStatusApplied, JobStatusRebased} {
		var jobID int64
		err := db.QueryRow(`
			INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type)
			VALUES (?, ?, ?, ?, 'design') RETURNING id
		`, repoID, commitID, "ref-"+string(status), string(status)).Scan(&jobID)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO ci_pr_batch_jobs (batch_id, job_id) VALUES (?, ?)`, batchID, jobID)
		require.NoError(t, err)
	}

	batch, err := db.ReconcileBatch(batchID)
	require.NoError(t, err)
	assert.Equal(t, 2, batch.CompletedJobs, "applied and rebased must count as completed")
	assert.Equal(t, 0, batch.FailedJobs)
}

func TestStaleAndMeaningfulQueries_IncludeAppliedRebased(t *testing.T) {
	// Regression: terminal/meaningful sets across GetStaleBatches,
	// GetNonTerminalBatchJobIDs, HasMeaningfulBatchResult, and
	// GetExpiredBatches must mirror AttachJobAndBumpTotal/
	// ReconcileBatch. If they diverged, a batch containing only
	// applied/rebased jobs could stay pending forever.
	db := openTestDB(t)
	defer db.Close()

	repoID := createRepo(t, db, "/tmp/repo-terminal-sets").ID
	commitID := createCommit(t, db, repoID, "f00dbabe").ID

	for _, status := range []JobStatus{JobStatusApplied, JobStatusRebased} {
		t.Run(string(status), func(t *testing.T) {
			var batchID int64
			err := db.QueryRow(`
				INSERT INTO ci_pr_batches (github_repo, pr_number, head_sha, total_jobs, created_at)
				VALUES ('x/y', 7, ?, 1, datetime('now', '-10 minutes'))
				RETURNING id
			`, "sha-"+string(status)).Scan(&batchID)
			require.NoError(t, err)

			var jobID int64
			err = db.QueryRow(`
				INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type)
				VALUES (?, ?, ?, ?, 'design') RETURNING id
			`, repoID, commitID, "gitref-"+string(status), string(status)).Scan(&jobID)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO ci_pr_batch_jobs (batch_id, job_id) VALUES (?, ?)`, batchID, jobID)
			require.NoError(t, err)

			nonTerm, err := db.GetNonTerminalBatchJobIDs(batchID)
			require.NoError(t, err)
			assert.Empty(t, nonTerm, "%s must count as terminal", status)

			meaningful, err := db.HasMeaningfulBatchResult(batchID)
			require.NoError(t, err)
			assert.True(t, meaningful, "%s must count as meaningful", status)

			stale, err := db.GetStaleBatches()
			require.NoError(t, err)
			found := false
			for _, b := range stale {
				if b.ID == batchID {
					found = true
					break
				}
			}
			assert.True(t, found, "batch with only %s must be stale-recoverable", status)
		})
	}
}

func TestHasMeaningfulBatchResult_CountsSkipped(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repoID := createRepo(t, db, "/tmp/repo-meaningful").ID
	commitID := createCommit(t, db, repoID, "dead").ID

	var jobID int64
	err := db.QueryRow(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type)
		VALUES (?, ?, 'dead', 'skipped', 'design') RETURNING id
	`, repoID, commitID).Scan(&jobID)
	require.NoError(t, err)

	var batchID int64
	err = db.QueryRow(`
		INSERT INTO ci_pr_batches (github_repo, pr_number, head_sha, total_jobs)
		VALUES ('x/y', 3, 'dead', 1) RETURNING id
	`).Scan(&batchID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO ci_pr_batch_jobs (batch_id, job_id) VALUES (?, ?)`, batchID, jobID)
	require.NoError(t, err)

	meaningful, err := db.HasMeaningfulBatchResult(batchID)
	require.NoError(t, err)
	assert.True(t, meaningful, "batch with a skipped auto-design row has a meaningful terminal outcome")
}
