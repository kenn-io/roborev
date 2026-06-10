package storage

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type jobEnv struct {
	db     *DB
	repo   *Repo
	commit *Commit
	job    *ReviewJob
}

func setupJobEnv(
	t *testing.T, repoPath, gitRef string,
) jobEnv {
	t.Helper()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo, commit, job := createJobChain(t, db, repoPath, gitRef)
	return jobEnv{
		db:     db,
		repo:   repo,
		commit: commit,
		job:    job,
	}
}

func TestJobLifecycle(t *testing.T) {
	env := setupJobEnv(t, "/tmp/test-repo", "abc123")

	assert.Equal(t, JobStatusQueued, env.job.Status)

	// Claim job
	claimed := claimJob(t, env.db, "worker-1")
	assert.Equal(t, claimed.ID, env.job.ID)
	assert.Equal(t, JobStatusRunning, claimed.Status)

	// Claim again should return nil (no more jobs)
	claimed2, err := env.db.ClaimJob("worker-2")
	require.NoError(t, err, "ClaimJob (second) failed")
	assert.Nil(t, claimed2)

	// Complete job
	require.NoError(t, env.db.CompleteJob(
		env.job.ID, "codex", "test prompt", "test output",
	), "CompleteJob failed")

	// Verify job status
	updatedJob, err := env.db.GetJobByID(env.job.ID)
	require.NoError(t, err, "GetJobByID failed")

	assert.Equal(t, JobStatusDone, updatedJob.Status)
}

func TestJobFailure(t *testing.T) {
	env := setupJobEnv(t, "/tmp/test-repo", "def456")
	claimJob(t, env.db, "worker-1")

	// Fail the job
	_, err := env.db.FailJob(env.job.ID, "", "test error message")
	require.NoError(t, err, "FailJob failed")

	updatedJob, err := env.db.GetJobByID(env.job.ID)
	require.NoError(t, err, "GetJobByID failed")

	assert.Equal(t, JobStatusFailed, updatedJob.Status)
	assert.Equal(t, "test error message", updatedJob.Error)
}

func TestFailJobOwnerScoped(t *testing.T) {
	env := setupJobEnv(t, "/tmp/test-repo", "fail-owner")
	claimJob(t, env.db, "worker-1")

	// Wrong worker should not be able to fail the job
	updated, err := env.db.FailJob(env.job.ID, "worker-2", "stale fail")
	require.NoError(t, err, "FailJob with wrong worker failed")

	assert.False(t, updated)

	// Job should still be running
	j, err := env.db.GetJobByID(env.job.ID)
	require.NoError(t, err, "GetJobByID failed")

	assert.Equal(t, JobStatusRunning, j.Status)

	// Correct worker should succeed
	updated, err = env.db.FailJob(env.job.ID, "worker-1", "legit fail")
	require.NoError(t, err, "FailJob with correct worker failed")

	assert.True(t, updated)

	j, err = env.db.GetJobByID(env.job.ID)
	require.NoError(t, err, "GetJobByID failed")

	assert.Equal(t, JobStatusFailed, j.Status)
	assert.Equal(t, "legit fail", j.Error)
}

func TestRetryJobOwnerScoped(t *testing.T) {
	env := setupJobEnv(t, "/tmp/test-repo", "retry-owner")
	claimJob(t, env.db, "worker-1")

	// Wrong worker should not be able to retry the job
	retried, err := env.db.RetryJob(env.job.ID, "worker-2", 3, 0)
	require.NoError(t, err, "RetryJob with wrong worker failed")

	assert.False(t, retried)

	// Job should still be running (not requeued)
	j, err := env.db.GetJobByID(env.job.ID)
	require.NoError(t, err, "GetJobByID failed")

	assert.Equal(t, JobStatusRunning, j.Status)

	// Correct worker should succeed
	retried, err = env.db.RetryJob(env.job.ID, "worker-1", 3, 0)
	require.NoError(t, err, "RetryJob with correct worker failed")

	assert.True(t, retried)

	j, err = env.db.GetJobByID(env.job.ID)
	require.NoError(t, err, "GetJobByID failed")

	assert.Equal(t, JobStatusQueued, j.Status)
}

func TestReviewOperations(t *testing.T) {
	env := setupJobEnv(t, "/tmp/test-repo", "rev123")
	claimJob(t, env.db, "worker-1")
	require.NoError(t, env.db.CompleteJob(
		env.job.ID, "codex", "the prompt", "the review output",
	), "CompleteJob failed")

	// Get review by commit SHA
	review, err := env.db.GetReviewByCommitSHA("rev123")
	require.NoError(t, err, "GetReviewByCommitSHA failed")

	assert.Equal(t, "the review output", review.Output)
	assert.Equal(t, "codex", review.Agent)
}

func TestReviewVerdictComputation(t *testing.T) {
	t.Run("verdict populated when output exists and no error", func(t *testing.T) {
		env := setupJobEnv(t, "/tmp/test-repo", "verdict-pass")
		_, err := env.db.ClaimJob("worker-1")
		require.NoError(t, err)
		require.NoError(t, env.db.CompleteJob(
			env.job.ID, "codex", "the prompt",
			"No issues found. The code looks good.",
		))

		review, err := env.db.GetReviewByJobID(env.job.ID)
		require.NoError(t, err, "GetReviewByJobID failed")

		assert.NotNil(t, review.Job.Verdict)
		assert.Equal(t, "P", *review.Job.Verdict)
	})

	t.Run("compact no remaining output stores pass verdict", func(t *testing.T) {
		env := setupJobEnv(t, "/tmp/test-repo", "verdict-compact-clean")
		_, err := env.db.ClaimJob("worker-1")
		require.NoError(t, err)
		require.NoError(t, env.db.CompleteJob(
			env.job.ID, "codex", "the prompt",
			"## Compact Analysis\n\n---\n\nNo remaining findings.",
		))

		review, err := env.db.GetReviewByJobID(env.job.ID)
		require.NoError(t, err, "GetReviewByJobID failed")

		assert.NotNil(t, review.Job.Verdict)
		assert.Equal(t, "P", *review.Job.Verdict)

		var vb sql.NullInt64
		err = env.db.QueryRow(
			`SELECT verdict_bool FROM reviews WHERE job_id = ?`,
			env.job.ID,
		).Scan(&vb)
		require.NoError(t, err)
		assert.True(t, vb.Valid, "verdict_bool should be set")
		assert.Equal(t, int64(1), vb.Int64)
	})

	t.Run("verdict nil when output is empty", func(t *testing.T) {
		env := setupJobEnv(t, "/tmp/test-repo", "verdict-empty")
		_, err := env.db.ClaimJob("worker-1")
		require.NoError(t, err)
		require.NoError(t, env.db.CompleteJob(
			env.job.ID, "codex", "the prompt", "",
		)) // empty output

		review, err := env.db.GetReviewByJobID(env.job.ID)
		require.NoError(t, err, "GetReviewByJobID failed")

		assert.Nil(t, review.Job.Verdict)

		// Verify verdict_bool is NULL in DB (not a false fail)
		var vb sql.NullInt64
		err = env.db.QueryRow(
			`SELECT verdict_bool FROM reviews WHERE job_id = ?`,
			env.job.ID,
		).Scan(&vb)
		require.NoError(t, err)
		assert.False(t, vb.Valid,
			"verdict_bool should be NULL for empty output")
	})

	t.Run("verdict nil when job has error", func(t *testing.T) {
		env := setupJobEnv(t, "/tmp/test-repo", "verdict-error")
		_, err := env.db.ClaimJob("worker-1")
		require.NoError(t, err)
		_, err = env.db.FailJob(
			env.job.ID, "", "API rate limit exceeded",
		)
		require.NoError(t, err)

		// Manually insert a review to simulate edge case
		_, err = env.db.Exec(
			`INSERT INTO reviews (job_id, agent, prompt, output) VALUES (?, 'codex', 'prompt', 'No issues found.')`,
			env.job.ID,
		)
		require.NoError(t, err, "Failed to insert review")

		review, err := env.db.GetReviewByJobID(env.job.ID)
		require.NoError(t, err, "GetReviewByJobID failed")

		assert.Nil(t, review.Job.Verdict)
	})

	t.Run("GetReviewByCommitSHA also respects verdict guard", func(t *testing.T) {
		env := setupJobEnv(t, "/tmp/test-repo", "verdict-sha")
		_, err := env.db.ClaimJob("worker-1")
		require.NoError(t, err)
		require.NoError(t, env.db.CompleteJob(
			env.job.ID, "codex", "the prompt", "No issues found.",
		))

		review, err := env.db.GetReviewByCommitSHA("verdict-sha")
		require.NoError(t, err, "GetReviewByCommitSHA failed")

		assert.NotNil(t, review.Job.Verdict)
		assert.Equal(t, "P", *review.Job.Verdict)
	})
}

func TestResponseOperations(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	commit, _ := db.GetOrCreateCommit(repo.ID, "resp123", "Author", "Subject", time.Now())

	// Add comment
	resp, err := db.AddComment(commit.ID, "test-user", "LGTM!")
	require.NoError(t, err, "AddComment failed: %v")

	assert.Equal(t, "LGTM!", resp.Response)

	// Get comments
	comments, err := db.GetCommentsForCommit(commit.ID)
	require.NoError(t, err, "GetCommentsForCommit failed: %v")

	assert.Len(t, comments, 1)
}

func TestMarkReviewClosed(t *testing.T) {
	env := setupJobEnv(t, "/tmp/test-repo", "addr123")
	_, err := env.db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.NoError(t, env.db.CompleteJob(
		env.job.ID, "codex", "prompt", "output",
	))

	// Get the review
	review, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err, "GetReviewByJobID failed")

	// Initially not closed
	assert.False(t, review.Closed)

	// Mark as closed
	err = env.db.MarkReviewClosed(review.ID, true)
	require.NoError(t, err, "MarkReviewClosed failed")

	// Verify it's closed
	updated, err := env.db.GetReviewByID(review.ID)
	require.NoError(t, err)
	assert.True(t, updated.Closed, "Review should be closed after MarkReviewClosed(true)")

	// Mark as open
	err = env.db.MarkReviewClosed(review.ID, false)
	require.NoError(t, err, "MarkReviewClosed(false) failed")

	updated2, err := env.db.GetReviewByID(review.ID)
	require.NoError(t, err)
	assert.False(t, updated2.Closed, "Review should not be closed after MarkReviewClosed(false)")
}

func TestMarkReviewClosedNotFound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Try to mark a non-existent review
	err := db.MarkReviewClosed(999999, true)
	require.Error(t, err)

	// Should be sql.ErrNoRows
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestMarkReviewClosedByJobID(t *testing.T) {
	env := setupJobEnv(t, "/tmp/test-repo", "jobaddr123")
	_, err := env.db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.NoError(t, env.db.CompleteJob(
		env.job.ID, "codex", "prompt", "output",
	))

	// Get the review to verify initial state
	review, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err, "GetReviewByJobID failed")

	// Initially not closed
	assert.False(t, review.Closed)

	// Mark as closed using job ID
	err = env.db.MarkReviewClosedByJobID(env.job.ID, true)
	require.NoError(t, err, "MarkReviewClosedByJobID failed")

	// Verify it's closed
	updated, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err)
	assert.True(t, updated.Closed, "Review should be closed after MarkReviewClosedByJobID(true)")

	// Mark as open using job ID
	err = env.db.MarkReviewClosedByJobID(env.job.ID, false)
	require.NoError(t, err, "MarkReviewClosedByJobID(false) failed")

	updated2, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err)
	assert.False(t, updated2.Closed, "Review should not be closed after MarkReviewClosedByJobID(false)")
}

func TestMarkReviewClosedByJobIDNotFound(t *testing.T) {
	env := setupJobEnv(t, "/tmp/test-repo", "jobaddr-missing")

	// Try to mark a non-existent job
	err := env.db.MarkReviewClosedByJobID(999999, true)
	require.Error(t, err)

	// Should be sql.ErrNoRows
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestRetryJob(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, _, job := createJobChain(t, db, "/tmp/test-repo", "retry123")

	// Claim the job (makes it running)
	claimJob(t, db, "worker-1")

	// A prior attempt's agent_invoked marker must not survive the retry: it
	// gates cost eligibility, so a stale marker could miscount a re-attempt
	// that fails before selecting an agent.
	require.NoError(t, db.MarkJobAgentInvoked(job.ID, "worker-1", "codex review retry123"))

	// Retry should succeed (retry_count: 0 -> 1)
	retried, err := db.RetryJob(job.ID, "", 3, 0)
	require.NoError(t, err, "RetryJob failed: %v")

	assert.True(t, retried)

	// Verify job is queued with retry_count=1 and the agent-ran marker cleared
	updatedJob, _ := db.GetJobByID(job.ID)
	assert.Equal(t, JobStatusQueued, updatedJob.Status)
	assert.Empty(t, updatedJob.CommandLine, "retry clears the prior attempt's command line")
	assert.False(t, getJobAgentInvoked(t, db, job.ID), "retry clears the agent_invoked marker")
	count, _ := db.GetJobRetryCount(job.ID)
	assert.Equal(t, 1, count)

	// Claim again and retry twice more (retry_count: 1->2, 2->3)
	_, _ = db.ClaimJob("worker-1")
	db.RetryJob(job.ID, "", 3, 0) // retry_count becomes 2
	_, _ = db.ClaimJob("worker-1")
	db.RetryJob(job.ID, "", 3, 0) // retry_count becomes 3

	count, _ = db.GetJobRetryCount(job.ID)
	assert.Equal(t, 3, count)

	// Claim again - next retry should fail (at max)
	_, _ = db.ClaimJob("worker-1")
	retried, err = db.RetryJob(job.ID, "", 3, 0)
	require.NoError(t, err, "RetryJob at max failed: %v")

	assert.False(t, retried)

	// Job should still be running (retry didn't happen)
	updatedJob, _ = db.GetJobByID(job.ID)
	assert.Equal(t, JobStatusRunning, updatedJob.Status)
}

func TestRetryJobOnlyWorksForRunning(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, _, job := createJobChain(t, db, "/tmp/test-repo", "retry-status")

	// Try to retry a queued job (should fail - not running)
	retried, err := db.RetryJob(job.ID, "", 3, 0)
	require.NoError(t, err, "RetryJob on queued job failed: %v")

	assert.False(t, retried)

	// Claim, complete, then try retry (should fail - job is done)
	_, _ = db.ClaimJob("worker-1")
	db.CompleteJob(job.ID, "codex", "p", "o")

	retried, err = db.RetryJob(job.ID, "", 3, 0)
	require.NoError(t, err, "RetryJob on done job failed: %v")

	assert.False(t, retried)
}

func TestRetryJobAtomic(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, _, job := createJobChain(t, db, "/tmp/test-repo", "retry-atomic")
	claimJob(t, db, "worker-1")

	// Simulate two concurrent retries - only first should succeed
	// (In practice this tests the atomic update)
	retried1, _ := db.RetryJob(job.ID, "", 3, 0)
	retried2, _ := db.RetryJob(job.ID, "", 3, 0) // Job is now queued, not running

	assert.True(t, retried1)
	assert.False(t, retried2, "Second retry should fail (job is no longer running)")

	// Verify retry_count is 1, not 2
	count, _ := db.GetJobRetryCount(job.ID)
	assert.Equal(t, 1, count)
}

func TestRetryJobBackoffDefersClaim(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, _, job := createJobChain(t, db, "/tmp/test-repo", "retry-backoff")
	claimJob(t, db, "worker-1")

	// Long backoff so the test isn't racing the wall clock — we only
	// check that ClaimJob refuses to return jobs whose retry_not_before
	// hasn't elapsed.
	retried, err := db.RetryJob(job.ID, "worker-1", 3, time.Hour)
	require.NoError(t, err)
	require.True(t, retried)

	skipped, err := db.ClaimJob("worker-2")
	require.NoError(t, err)
	assert.Nil(t, skipped, "ClaimJob must not return jobs still in their retry backoff window")

	// Once the gate is in the past, the same job is claimable. Set the
	// column directly instead of sleeping a real backoff — we're testing
	// the predicate, not the clock.
	past := retryNotBeforeAt(time.Now().Add(-time.Minute))
	_, err = db.Exec(`UPDATE review_jobs SET retry_not_before = ? WHERE id = ?`, past, job.ID)
	require.NoError(t, err)

	claimed, err := db.ClaimJob("worker-2")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, job.ID, claimed.ID)
}

func TestRetryJobZeroBackoffLeavesNotBeforeNull(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, _, job := createJobChain(t, db, "/tmp/test-repo", "retry-no-backoff")
	claimJob(t, db, "worker-1")

	retried, err := db.RetryJob(job.ID, "worker-1", 3, 0)
	require.NoError(t, err)
	require.True(t, retried)

	var stored sql.NullString
	require.NoError(t, db.QueryRow(`SELECT retry_not_before FROM review_jobs WHERE id = ?`, job.ID).Scan(&stored))
	assert.False(t, stored.Valid, "zero backoff should leave retry_not_before NULL")

	claimed, err := db.ClaimJob("worker-2")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, job.ID, claimed.ID)
}

func TestFailoverJob(t *testing.T) {
	t.Run("succeeds with backup agent", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/failover-repo")
		commit := createCommit(t, db, repo.ID, "fo-abc123")

		job, err := db.EnqueueJob(EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit.ID,
			GitRef:   "fo-abc123",
			Agent:    "primary",
		})
		require.NoError(t, err, "EnqueueJob: %v")

		// Claim to make it running
		claimJob(t, db, "worker-1")
		require.NoError(t, db.MarkJobAgentInvoked(job.ID, "worker-1", "primary review fo-abc123"))

		// Failover should succeed
		ok, err := db.FailoverJob(job.ID, "worker-1", "backup", "")
		require.NoError(t, err, "FailoverJob: %v")

		assert.True(t, ok)

		// Verify: agent swapped, retry_count reset, status queued, marker cleared
		updated, err := db.GetJobByID(job.ID)
		require.NoError(t, err, "GetJobByID: %v")

		assert.Equal(t, "backup", updated.Agent)
		assert.Equal(t, JobStatusQueued, updated.Status)
		assert.Empty(t, updated.CommandLine, "failover clears the prior agent's command line")
		assert.False(t, getJobAgentInvoked(t, db, job.ID), "failover clears the agent_invoked marker")
		count, _ := db.GetJobRetryCount(job.ID)
		assert.Equal(t, 0, count)
	})

	t.Run("clears retry_not_before on failover", func(t *testing.T) {
		// Switching agents resets the retry path, so any prior backoff
		// gate left by RetryJob should be cleared. Otherwise the new
		// agent can't even be claimed until the old gate expires.
		db := openTestDB(t)
		defer db.Close()

		_, _, job := createJobChain(t, db, "/tmp/failover-backoff", "fo-backoff")
		claimJob(t, db, "worker-1")

		// Stamp a far-future retry_not_before to simulate a job that
		// hit its retry backoff right before failover took over.
		future := time.Now().Add(30 * time.Second).Format(time.RFC3339)
		_, err := db.Exec(`UPDATE review_jobs SET retry_not_before = ? WHERE id = ?`, future, job.ID)
		require.NoError(t, err)

		ok, err := db.FailoverJob(job.ID, "worker-1", "backup", "")
		require.NoError(t, err)
		require.True(t, ok)

		claimed, err := db.ClaimJob("worker-2")
		require.NoError(t, err)
		require.NotNil(t, claimed, "failover should clear retry_not_before so the new agent is claimable")
		assert.Equal(t, "backup", claimed.Agent)
	})

	t.Run("clears model on failover", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/failover-model")
		commit := createCommit(t, db, repo.ID, "fo-model")

		job, err := db.EnqueueJob(EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit.ID,
			GitRef:   "fo-model",
			Agent:    "primary",
			Model:    "o3-mini",
		})
		require.NoError(t, err, "EnqueueJob: %v")

		assert.Equal(t, "o3-mini", job.Model)

		claimJob(t, db, "worker-1")

		ok, err := db.FailoverJob(job.ID, "worker-1", "backup", "")
		require.NoError(t, err, "FailoverJob: %v")

		assert.True(t, ok)

		updated, err := db.GetJobByID(job.ID)
		require.NoError(t, err, "GetJobByID: %v")

		assert.Empty(t, updated.Model)
	})

	t.Run("sets backup model on failover", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/failover-bmodel")
		commit := createCommit(t, db, repo.ID, "fo-bmodel")

		job, err := db.EnqueueJob(EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit.ID,
			GitRef:   "fo-bmodel",
			Agent:    "primary",
			Model:    "o3-mini",
		})
		require.NoError(t, err, "EnqueueJob: %v")

		claimJob(t, db, "worker-1")

		ok, err := db.FailoverJob(job.ID, "worker-1", "backup", "claude-sonnet")
		require.NoError(t, err, "FailoverJob: %v")

		assert.True(t, ok)

		updated, err := db.GetJobByID(job.ID)
		require.NoError(t, err, "GetJobByID: %v")

		assert.Equal(t, "claude-sonnet", updated.Model)
		assert.Equal(t, "backup", updated.Agent)
	})

	t.Run("fails with empty backup agent", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		_, _, job := createJobChain(t, db, "/tmp/failover-nobackup", "fo-no-backup")
		claimJob(t, db, "worker-1")

		ok, err := db.FailoverJob(job.ID, "worker-1", "", "")
		require.NoError(t, err, "FailoverJob: %v")

		assert.False(t, ok)
	})

	t.Run("fails when backup equals agent", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/failover-same")
		commit := createCommit(t, db, repo.ID, "fo-same123")

		job, err := db.EnqueueJob(EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit.ID,
			GitRef:   "fo-same123",
			Agent:    "codex",
		})
		require.NoError(t, err, "EnqueueJob: %v")

		claimJob(t, db, "worker-1")

		ok, err := db.FailoverJob(job.ID, "worker-1", "codex", "")
		require.NoError(t, err, "FailoverJob: %v")

		assert.False(t, ok, "Expected failover to return false when backup == agent")
	})

	t.Run("fails when not running", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/failover-queued")
		commit := createCommit(t, db, repo.ID, "fo-queued")

		// Job is queued (not claimed/running)
		job, err := db.EnqueueJob(EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit.ID,
			GitRef:   "fo-queued",
			Agent:    "primary",
		})
		require.NoError(t, err, "EnqueueJob: %v")

		ok, err := db.FailoverJob(job.ID, "worker-1", "backup", "")
		require.NoError(t, err, "FailoverJob: %v")

		assert.False(t, ok)
	})

	t.Run("second failover with same backup is no-op", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/failover-double")
		commit := createCommit(t, db, repo.ID, "fo-double")

		job, err := db.EnqueueJob(EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit.ID,
			GitRef:   "fo-double",
			Agent:    "primary",
		})
		require.NoError(t, err, "EnqueueJob: %v")

		claimJob(t, db, "worker-1")

		// First failover: primary -> backup
		db.FailoverJob(job.ID, "worker-1", "backup", "")

		// Reclaim, now agent is "backup"
		claimJob(t, db, "worker-1")

		// Second failover with same backup agent should fail (agent == backup)
		ok, err := db.FailoverJob(job.ID, "worker-1", "backup", "")
		require.NoError(t, err, "FailoverJob second attempt: %v")

		assert.False(t, ok, "Expected second failover to return false (agent already is backup)")
	})

	t.Run("fails when wrong worker", func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()

		repo := createRepo(t, db, "/tmp/failover-wrongworker")
		commit := createCommit(t, db, repo.ID, "fo-wrongw")

		job, err := db.EnqueueJob(EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit.ID,
			GitRef:   "fo-wrongw",
			Agent:    "primary",
		})
		require.NoError(t, err, "EnqueueJob: %v")

		claimJob(t, db, "worker-1")

		// A different worker should not be able to failover this job
		ok, err := db.FailoverJob(job.ID, "worker-2", "backup", "")
		require.NoError(t, err, "FailoverJob: %v")

		assert.False(t, ok)

		// Verify original agent is unchanged
		updated, err := db.GetJobByID(job.ID)
		require.NoError(t, err, "GetJobByID: %v")

		assert.Equal(t, "primary", updated.Agent)
	})
}

func TestCancelJob(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	t.Run("cancel queued job", func(t *testing.T) {
		_, _, job := createJobChain(t, db, "/tmp/test-repo", "cancel-queued")

		err := db.CancelJob(job.ID)
		require.NoError(t, err, "CancelJob failed: %v")

		updated, _ := db.GetJobByID(job.ID)
		assert.Equal(t, JobStatusCanceled, updated.Status)
	})

	t.Run("cancel running job", func(t *testing.T) {
		_, _, job := createJobChain(t, db, "/tmp/test-repo", "cancel-running")
		db.ClaimJob("worker-1")

		err := db.CancelJob(job.ID)
		require.NoError(t, err, "CancelJob failed: %v")

		updated, _ := db.GetJobByID(job.ID)
		assert.Equal(t, JobStatusCanceled, updated.Status)
	})

	t.Run("cancel done job fails", func(t *testing.T) {
		_, _, job := createJobChain(t, db, "/tmp/test-repo", "cancel-done")
		db.ClaimJob("worker-1")
		db.CompleteJob(job.ID, "codex", "prompt", "output")

		err := db.CancelJob(job.ID)
		require.Error(t, err)
	})

	t.Run("cancel failed job fails", func(t *testing.T) {
		_, _, job := createJobChain(t, db, "/tmp/test-repo", "cancel-failed")
		db.ClaimJob("worker-1")
		db.FailJob(job.ID, "", "some error")

		err := db.CancelJob(job.ID)
		require.Error(t, err)
	})

	t.Run("complete respects canceled status", func(t *testing.T) {
		_, _, job := createJobChain(t, db, "/tmp/test-repo", "complete-canceled")
		db.ClaimJob("worker-1")
		db.CancelJob(job.ID)

		// CompleteJob should not overwrite canceled status
		db.CompleteJob(job.ID, "codex", "prompt", "output")

		updated, _ := db.GetJobByID(job.ID)
		assert.Equal(t, JobStatusCanceled, updated.Status)

		// Verify no review was inserted (should get sql.ErrNoRows)
		_, err := db.GetReviewByJobID(job.ID)
		require.Error(t, err)
		require.ErrorIs(t, err, sql.ErrNoRows, "expected no rows error, got: %v", err)
	})

	t.Run("fail respects canceled status", func(t *testing.T) {
		_, _, job := createJobChain(t, db, "/tmp/test-repo", "fail-canceled")
		db.ClaimJob("worker-1")
		db.CancelJob(job.ID)

		// FailJob should not overwrite canceled status
		db.FailJob(job.ID, "", "some error")

		updated, _ := db.GetJobByID(job.ID)
		assert.Equal(t, JobStatusCanceled, updated.Status)
	})

	t.Run("canceled jobs counted correctly", func(t *testing.T) {
		// Create and cancel a new job
		_, _, job := createJobChain(t, db, "/tmp/test-repo", "cancel-count")
		db.CancelJob(job.ID)

		_, _, _, _, canceled, _, _, _, err := db.GetJobCounts()
		require.NoError(t, err, "GetJobCounts failed: %v")

		assert.GreaterOrEqual(t, canceled, 1)
	})
}

func TestMarkJobApplied(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	commit, _ := db.GetOrCreateCommit(repo.ID, "applied-test", "A", "S", time.Now())

	t.Run("mark done fix job as applied", func(t *testing.T) {
		job, _ := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "applied-test", Agent: "codex", JobType: JobTypeFix, ParentJobID: 1})
		db.ClaimJob("worker-1")
		db.CompleteJob(job.ID, "codex", "prompt", "output")

		err := db.MarkJobApplied(job.ID)
		require.NoError(t, err, "MarkJobApplied failed: %v")

		updated, _ := db.GetJobByID(job.ID)
		assert.Equal(t, JobStatusApplied, updated.Status)
	})

	t.Run("mark non-done job fails", func(t *testing.T) {
		job, _ := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "applied-test-q", Agent: "codex", JobType: JobTypeFix, ParentJobID: 1})

		err := db.MarkJobApplied(job.ID)
		require.Error(t, err)
	})

	t.Run("mark applied job again fails", func(t *testing.T) {
		job, _ := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "applied-test-2", Agent: "codex", JobType: JobTypeFix, ParentJobID: 1})
		db.ClaimJob("worker-1")
		db.CompleteJob(job.ID, "codex", "prompt", "output")
		db.MarkJobApplied(job.ID)

		err := db.MarkJobApplied(job.ID)
		require.Error(t, err)
	})

	t.Run("mark non-fix job fails", func(t *testing.T) {
		job, _ := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "applied-review", Agent: "codex"})
		db.ClaimJob("worker-1")
		db.CompleteJob(job.ID, "codex", "prompt", "output")

		err := db.MarkJobApplied(job.ID)
		require.Error(t, err)
	})
}

func TestMarkJobRebased(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	commit, _ := db.GetOrCreateCommit(repo.ID, "rebased-test", "A", "S", time.Now())

	t.Run("mark done fix job as rebased", func(t *testing.T) {
		job, _ := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "rebased-test", Agent: "codex", JobType: JobTypeFix, ParentJobID: 1})
		db.ClaimJob("worker-1")
		db.CompleteJob(job.ID, "codex", "prompt", "output")

		err := db.MarkJobRebased(job.ID)
		require.NoError(t, err, "MarkJobRebased failed: %v")

		updated, _ := db.GetJobByID(job.ID)
		assert.Equal(t, JobStatusRebased, updated.Status)
	})

	t.Run("mark non-done job fails", func(t *testing.T) {
		job, _ := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "rebased-test-q", Agent: "codex", JobType: JobTypeFix, ParentJobID: 1})

		err := db.MarkJobRebased(job.ID)
		require.Error(t, err)
	})

	t.Run("mark non-fix job fails", func(t *testing.T) {
		job, _ := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "rebased-review", Agent: "codex"})
		db.ClaimJob("worker-1")
		db.CompleteJob(job.ID, "codex", "prompt", "output")

		err := db.MarkJobRebased(job.ID)
		require.Error(t, err)
	})
}

func TestReenqueueJob(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	t.Run("rerun failed job", func(t *testing.T) {
		_, _, job := createJobChain(t, db, "/tmp/test-repo", "rerun-failed")
		db.ClaimJob("worker-1")
		db.FailJob(job.ID, "", "some error")

		err := db.ReenqueueJob(job.ID, ReenqueueOpts{})
		require.NoError(t, err, "ReenqueueJob failed: %v")

		updated, _ := db.GetJobByID(job.ID)
		assert.Equal(t, JobStatusQueued, updated.Status)
		assert.Empty(t, updated.Error)
		assert.Nil(t, updated.StartedAt)
		assert.Nil(t, updated.FinishedAt)
	})

	t.Run("rerun canceled job", func(t *testing.T) {
		_, _, job := createJobChain(t, db, "/tmp/test-repo", "rerun-canceled")
		db.CancelJob(job.ID)

		err := db.ReenqueueJob(job.ID, ReenqueueOpts{})
		require.NoError(t, err, "ReenqueueJob failed: %v")

		updated, _ := db.GetJobByID(job.ID)
		assert.Equal(t, JobStatusQueued, updated.Status)
	})

	t.Run("rerun done job", func(t *testing.T) {
		_, _, job := createJobChain(t, db, "/tmp/test-repo", "rerun-done")
		// ClaimJob returns the claimed job; keep claiming until we get ours
		var claimed *ReviewJob
		for {
			claimed, _ = db.ClaimJob("worker-1")
			assert.NotNil(t, claimed)
			if claimed.ID == job.ID {
				break
			}
			// Complete other jobs to clear them
			db.CompleteJob(claimed.ID, "codex", "prompt", "output")
		}
		db.CompleteJob(job.ID, "codex", "prompt", "output")

		err := db.ReenqueueJob(job.ID, ReenqueueOpts{})
		require.NoError(t, err, "ReenqueueJob failed: %v")

		updated, _ := db.GetJobByID(job.ID)
		assert.Equal(t, JobStatusQueued, updated.Status)
	})

	t.Run("rerun queued job fails", func(t *testing.T) {
		_, _, job := createJobChain(t, db, "/tmp/test-repo", "rerun-queued")

		err := db.ReenqueueJob(job.ID, ReenqueueOpts{})
		require.Error(t, err)
	})

	t.Run("rerun running job fails", func(t *testing.T) {
		_, _, job := createJobChain(t, db, "/tmp/test-repo", "rerun-running")
		db.ClaimJob("worker-1")

		err := db.ReenqueueJob(job.ID, ReenqueueOpts{})
		require.Error(t, err)
	})

	t.Run("rerun nonexistent job fails", func(t *testing.T) {
		err := db.ReenqueueJob(99999, ReenqueueOpts{})
		require.Error(t, err)
	})

	t.Run("rerun updates effective model and preserves requested model", func(t *testing.T) {
		isolatedDB := openTestDB(t)
		defer isolatedDB.Close()

		repo := createRepo(t, isolatedDB, "/tmp/rerun-requested-model")
		commit := createCommit(t, isolatedDB, repo.ID, "rerun-requested-model-sha")

		job, err := isolatedDB.EnqueueJob(EnqueueOpts{
			RepoID:            repo.ID,
			CommitID:          commit.ID,
			GitRef:            "rerun-requested-model-sha",
			Agent:             "opencode",
			Model:             "minimax-m2.5-free",
			RequestedModel:    "minimax-m2.5-free",
			RequestedProvider: "anthropic",
			Provider:          "anthropic",
		})
		require.NoError(t, err)

		claimed, err := isolatedDB.ClaimJob("worker-1")
		require.NoError(t, err)
		require.NotNil(t, claimed)
		assert.Equal(t, job.ID, claimed.ID)
		require.NoError(t, isolatedDB.CompleteJob(job.ID, "opencode", "prompt", "output"))

		err = isolatedDB.ReenqueueJob(job.ID, ReenqueueOpts{Model: "openai/gpt-5", Provider: "openai"})
		require.NoError(t, err)

		updated, err := isolatedDB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, JobStatusQueued, updated.Status)
		assert.Equal(t, "openai/gpt-5", updated.Model)
		assert.Equal(t, "openai", updated.Provider)
		assert.Equal(t, "minimax-m2.5-free", updated.RequestedModel)
		assert.Equal(t, "anthropic", updated.RequestedProvider)
	})

	t.Run("rerun preserves worktree_path", func(t *testing.T) {
		isolatedDB := openTestDB(t)
		defer isolatedDB.Close()

		repo := createRepo(t, isolatedDB, "/tmp/wt-preserve-repo")
		commit := createCommit(t, isolatedDB, repo.ID, "wt-preserve-sha")

		job, err := isolatedDB.EnqueueJob(EnqueueOpts{
			RepoID:       repo.ID,
			CommitID:     commit.ID,
			GitRef:       "wt-preserve-sha",
			Agent:        "test",
			WorktreePath: "/tmp/wt/feature-branch",
		})
		require.NoError(t, err)

		claimed, err := isolatedDB.ClaimJob("worker-1")
		require.NoError(t, err)
		require.NotNil(t, claimed)
		assert.Equal(t, job.ID, claimed.ID)

		err = isolatedDB.CompleteJob(job.ID, "test", "prompt", "output")
		require.NoError(t, err)

		err = isolatedDB.ReenqueueJob(job.ID, ReenqueueOpts{})
		require.NoError(t, err)

		updated, err := isolatedDB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, JobStatusQueued, updated.Status)
		assert.Equal(t, "/tmp/wt/feature-branch", updated.WorktreePath)
	})

	t.Run("rerun done job and complete again", func(t *testing.T) {
		// Use isolated database to avoid interference from other subtests
		isolatedDB := openTestDB(t)
		defer isolatedDB.Close()

		_, _, job := createJobChain(t, isolatedDB, "/tmp/isolated-repo", "rerun-complete-cycle")

		// First completion cycle
		claimed, _ := isolatedDB.ClaimJob("worker-1")
		assert.False(t, claimed == nil || claimed.ID != job.ID)
		err := isolatedDB.CompleteJob(job.ID, "codex", "first prompt", "first output")
		require.NoError(t, err, "First CompleteJob failed: %v")

		// Verify first review exists
		review1, err := isolatedDB.GetReviewByJobID(job.ID)
		require.NoError(t, err, "GetReviewByJobID failed after first complete: %v")

		assert.Equal(t, "first output", review1.Output)

		// Re-enqueue the done job
		err = isolatedDB.ReenqueueJob(job.ID, ReenqueueOpts{})
		require.NoError(t, err, "ReenqueueJob failed: %v")

		// Verify review was deleted
		_, err = isolatedDB.GetReviewByJobID(job.ID)
		require.Error(t, err, "Expected GetReviewByJobID to fail after re-enqueue (review should be deleted)")

		// Second completion cycle
		claimed, _ = isolatedDB.ClaimJob("worker-1")
		assert.False(t, claimed == nil || claimed.ID != job.ID)
		err = isolatedDB.CompleteJob(job.ID, "codex", "second prompt", "second output")
		require.NoError(t, err, "Second CompleteJob failed: %v")

		// Verify second review exists with new content
		review2, err := isolatedDB.GetReviewByJobID(job.ID)
		require.NoError(t, err, "GetReviewByJobID failed after second complete: %v")

		assert.Equal(t, "second output", review2.Output)
	})
}

func TestReenqueueJob_ClearsPrebuiltPrompt(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/rerun-prebuilt")
	commit := createCommit(t, db, repo.ID, "rerun-prebuilt-sha")

	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:         repo.ID,
		CommitID:       commit.ID,
		GitRef:         "base..rerun-prebuilt-sha",
		Agent:          "test",
		Prompt:         "prebuilt review prompt with discussion context",
		PromptPrebuilt: true,
		JobType:        JobTypeRange,
	})
	require.NoError(t, err)
	assert.True(t, job.PromptPrebuilt)
	assert.Equal(t, "prebuilt review prompt with discussion context", job.Prompt)

	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "test", job.Prompt, "review output"))

	err = db.ReenqueueJob(job.ID, ReenqueueOpts{})
	require.NoError(t, err)

	updated, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusQueued, updated.Status)
	assert.False(t, updated.PromptPrebuilt, "rerun should clear prompt_prebuilt")
	assert.Empty(t, updated.Prompt, "rerun should clear stored prompt")
}

func TestReenqueueJob_PreservesDirtyFiles(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/rerun-dirty-files")
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:     repo.ID,
		GitRef:     "dirty",
		Agent:      "test",
		JobType:    JobTypeDirty,
		DirtyFiles: []string{"frontend/package-lock.json"},
	})
	require.NoError(t, err)

	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", "review output"))

	err = db.ReenqueueJob(job.ID, ReenqueueOpts{})
	require.NoError(t, err)

	reclaimed, err := db.ClaimJob("worker-2")
	require.NoError(t, err)
	require.Equal(t, job.ID, reclaimed.ID)
	assert.Equal(t, []string{"frontend/package-lock.json"}, reclaimed.DirtyFiles)
	assert.Nil(t, reclaimed.DiffContent)
	assert.False(t, reclaimed.PromptPrebuilt)
}

func TestReenqueueJob_PreservesTaskPrompt(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/rerun-task")
	taskPrompt := "analyze the codebase for unused exports"

	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:  repo.ID,
		GitRef:  "prompt",
		Agent:   "test",
		Prompt:  taskPrompt,
		JobType: JobTypeTask,
	})
	require.NoError(t, err)
	assert.Equal(t, taskPrompt, job.Prompt)

	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "test", taskPrompt, "task output"))

	err = db.ReenqueueJob(job.ID, ReenqueueOpts{})
	require.NoError(t, err)

	updated, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusQueued, updated.Status)
	assert.Equal(t, taskPrompt, updated.Prompt, "rerun should preserve task prompt")
}

func TestEnqueueJobWithPatchID(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-patch-id")
	commit := createCommit(t, db, repo.ID, "abc123")

	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "abc123",
		Agent:    "test",
		PatchID:  "deadbeef1234",
	})
	require.NoError(t, err, "EnqueueJob: %v")

	assert.Equal(t, "deadbeef1234", job.PatchID)

	// Verify it round-trips through GetJobByID
	got, err := db.GetJobByID(job.ID)
	require.NoError(t, err, "GetJobByID: %v")

	assert.Equal(t, "deadbeef1234", got.PatchID)
}

func TestEnqueueJobWithCIBaseBranch(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-ci-base-branch")
	commit := createCommit(t, db, repo.ID, "abc123")

	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:       repo.ID,
		CommitID:     commit.ID,
		GitRef:       "abc123",
		Agent:        "test",
		CIBaseBranch: "main",
	})
	require.NoError(t, err)
	assert.Equal(t, "main", job.CIBaseBranch)
	assert.Empty(t, job.Branch, "CIBaseBranch must not populate Branch")

	got, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "main", got.CIBaseBranch)
	assert.Empty(t, got.Branch)

	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, "main", claimed.CIBaseBranch, "ClaimJob must hydrate CIBaseBranch for event broadcasts")
}

func TestHookBranchPrefersLocalBranch(t *testing.T) {
	assert.Equal(t, "feat/x", ReviewJob{Branch: "feat/x", CIBaseBranch: "main"}.HookBranch())
	assert.Equal(t, "main", ReviewJob{CIBaseBranch: "main"}.HookBranch())
	assert.Empty(t, ReviewJob{}.HookBranch())
}

func TestRemapJobGitRef(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-remap")
	commit := createCommit(t, db, repo.ID, "oldsha")

	t.Run("remap updates matching jobs", func(t *testing.T) {
		job, err := db.EnqueueJob(EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit.ID,
			GitRef:   "oldsha",
			Agent:    "test",
			PatchID:  "patchabc",
		})
		require.NoError(t, err, "EnqueueJob: %v")

		newCommit := createCommit(t, db, repo.ID, "newsha")
		n, err := db.RemapJobGitRef(repo.ID, "oldsha", "newsha", "patchabc", newCommit.ID)
		require.NoError(t, err, "RemapJobGitRef: %v")

		assert.Equal(t, 1, n)

		got, err := db.GetJobByID(job.ID)
		require.NoError(t, err, "GetJobByID: %v")

		assert.Equal(t, "newsha", got.GitRef)
	})

	t.Run("skips on patch_id mismatch", func(t *testing.T) {
		commit2 := createCommit(t, db, repo.ID, "sha2")
		_, err := db.EnqueueJob(EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit2.ID,
			GitRef:   "sha2",
			Agent:    "test",
			PatchID:  "patch_original",
		})
		require.NoError(t, err, "EnqueueJob: %v")

		newCommit := createCommit(t, db, repo.ID, "sha2_new")
		n, err := db.RemapJobGitRef(repo.ID, "sha2", "sha2_new", "patch_different", newCommit.ID)
		require.NoError(t, err, "RemapJobGitRef: %v")

		assert.Equal(t, 0, n)
	})

	t.Run("returns 0 for no matches", func(t *testing.T) {
		newCommit := createCommit(t, db, repo.ID, "nonexistent_new")
		n, err := db.RemapJobGitRef(repo.ID, "nonexistent", "nonexistent_new", "patch", newCommit.ID)
		require.NoError(t, err, "RemapJobGitRef: %v")

		assert.Equal(t, 0, n)
	})
}

func TestJobTypeBackfill(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/backfill-test")

	// Insert jobs with job_type='review' to simulate pre-migration state
	// 1. Normal commit review - should stay 'review'
	commit := createCommit(t, db, repo.ID, "abc123")
	_, err := db.Exec(`INSERT INTO review_jobs (repo_id, commit_id, git_ref, agent, status, job_type) VALUES (?, ?, 'abc123', 'codex', 'done', 'review')`,
		repo.ID, commit.ID)
	require.NoError(t, err, "insert review job: %v")

	// 2. Dirty job (git_ref='dirty') - should become 'dirty'
	_, err = db.Exec(`INSERT INTO review_jobs (repo_id, git_ref, agent, status, job_type) VALUES (?, 'dirty', 'codex', 'done', 'review')`, repo.ID)
	require.NoError(t, err, "insert dirty job: %v")

	// 3. Dirty job (diff_content set) - should become 'dirty'
	_, err = db.Exec(`INSERT INTO review_jobs (repo_id, git_ref, agent, status, job_type, diff_content) VALUES (?, 'some-ref', 'codex', 'done', 'review', 'diff here')`, repo.ID)
	require.NoError(t, err, "insert dirty-with-diff job: %v")

	// 4. Range job (git_ref has ..) - should become 'range'
	_, err = db.Exec(`INSERT INTO review_jobs (repo_id, git_ref, agent, status, job_type) VALUES (?, 'abc..def', 'codex', 'done', 'review')`, repo.ID)
	require.NoError(t, err, "insert range job: %v")

	// 5. Task job (no commit_id, no diff, non-dirty git_ref) - should become 'task'
	_, err = db.Exec(`INSERT INTO review_jobs (repo_id, git_ref, agent, status, job_type) VALUES (?, 'analyze', 'codex', 'done', 'review')`, repo.ID)
	require.NoError(t, err, "insert task job: %v")

	// Run backfill SQL (same as migration)
	_, err = db.Exec(`UPDATE review_jobs SET job_type = 'dirty' WHERE (git_ref = 'dirty' OR diff_content IS NOT NULL) AND job_type = 'review'`)
	require.NoError(t, err, "backfill dirty: %v")

	_, err = db.Exec(`UPDATE review_jobs SET job_type = 'range' WHERE git_ref LIKE '%..%' AND commit_id IS NULL AND job_type = 'review'`)
	require.NoError(t, err, "backfill range: %v")

	_, err = db.Exec(`UPDATE review_jobs SET job_type = 'task' WHERE commit_id IS NULL AND diff_content IS NULL AND git_ref != 'dirty' AND git_ref NOT LIKE '%..%' AND git_ref != '' AND job_type = 'review'`)
	require.NoError(t, err, "backfill task: %v")

	// Verify results
	rows, err := db.Query(`SELECT git_ref, job_type FROM review_jobs ORDER BY id`)
	require.NoError(t, err, "query jobs: %v")

	defer rows.Close()

	expected := []struct {
		gitRef  string
		jobType string
	}{
		{"abc123", "review"},
		{"dirty", "dirty"},
		{"some-ref", "dirty"},
		{"abc..def", "range"},
		{"analyze", "task"},
	}

	i := 0
	for rows.Next() {
		var gitRef, jobType string
		if err := rows.Scan(&gitRef, &jobType); err != nil {
			require.NoError(t, err, "scan row: %v")
		}
		assert.Less(t, i, len(expected), "more rows than expected")
		assert.False(t, gitRef != expected[i].gitRef || jobType != expected[i].jobType)
		i++
	}
	assert.Equal(t, len(expected), i)
}

func TestSaveJobSessionID_StaleWorkerIgnored(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, _, job := createJobChain(t, db, "/tmp/test-repo", "session-race")

	claimJob(t, db, "worker-A")

	err := db.SaveJobSessionID(job.ID, "worker-A", "session-A")
	require.NoError(t, err, "SaveJobSessionID (worker-A): %v", err)

	j, err := db.GetJobByID(job.ID)
	require.NoError(t, err, "GetJobByID after worker-A save: %v", err)
	assert.Equal(t, "session-A", j.SessionID)

	err = db.CancelJob(job.ID)
	require.NoError(t, err, "CancelJob: %v", err)

	err = db.ReenqueueJob(job.ID, ReenqueueOpts{})
	require.NoError(t, err, "ReenqueueJob: %v", err)

	j, err = db.GetJobByID(job.ID)
	require.NoError(t, err, "GetJobByID after reenqueue: %v", err)
	assert.Empty(t, j.SessionID)

	claimJob(t, db, "worker-B")

	err = db.SaveJobSessionID(job.ID, "worker-A", "stale-session")
	require.NoError(t, err, "SaveJobSessionID (stale worker-A): %v", err)

	j, err = db.GetJobByID(job.ID)
	require.NoError(t, err, "GetJobByID after stale worker save: %v", err)
	assert.Empty(t, j.SessionID)

	err = db.SaveJobSessionID(job.ID, "worker-B", "session-B")
	require.NoError(t, err, "SaveJobSessionID (worker-B): %v", err)

	j, err = db.GetJobByID(job.ID)
	require.NoError(t, err, "GetJobByID after worker-B save: %v", err)
	assert.Equal(t, "session-B", j.SessionID)

	err = db.SaveJobSessionID(job.ID, "worker-B", "session-B2")
	require.NoError(t, err, "SaveJobSessionID (worker-B second): %v", err)

	j, err = db.GetJobByID(job.ID)
	require.NoError(t, err, "GetJobByID after worker-B second save: %v", err)
	assert.Equal(t, "session-B", j.SessionID)
}

// TestMarkJobAgentInvoked_StaleWorkerIgnored verifies the agent-ran marker is
// scoped to the current attempt: a worker that calls MarkJobAgentInvoked after
// its attempt was canceled and re-claimed by another worker must not set the
// marker on the new attempt's row, which would wrongly make it cost-eligible.
func TestMarkJobAgentInvoked_StaleWorkerIgnored(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	defer db.Close()

	_, _, job := createJobChain(t, db, "/tmp/test-repo", "invoked-race")

	claimJob(t, db, "worker-A")
	require.NoError(t, db.MarkJobAgentInvoked(job.ID, "worker-A", "codex review x"),
		"MarkJobAgentInvoked (worker-A)")
	assert.True(getJobAgentInvoked(t, db, job.ID), "owning worker sets the marker")

	// Cancel + reenqueue hands the row to a new attempt and clears the marker.
	require.NoError(t, db.CancelJob(job.ID), "CancelJob")
	require.NoError(t, db.ReenqueueJob(job.ID, ReenqueueOpts{}), "ReenqueueJob")
	assert.False(getJobAgentInvoked(t, db, job.ID), "reenqueue clears the marker")

	claimJob(t, db, "worker-B")

	// The stale worker-A write must not land on the row worker-B now owns.
	require.NoError(t, db.MarkJobAgentInvoked(job.ID, "worker-A", "stale review x"),
		"MarkJobAgentInvoked (stale worker-A)")
	assert.False(getJobAgentInvoked(t, db, job.ID),
		"stale worker does not set the marker on the new attempt")

	// The current owner can still set it.
	require.NoError(t, db.MarkJobAgentInvoked(job.ID, "worker-B", "codex review x"),
		"MarkJobAgentInvoked (worker-B)")
	assert.True(getJobAgentInvoked(t, db, job.ID), "current owner sets the marker")
}

func TestMinSeverityRoundTrip(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/min-sev-test")
	commit := createCommit(t, db, repo.ID, "abc123")

	// Enqueue with min_severity set
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:      repo.ID,
		CommitID:    commit.ID,
		GitRef:      "abc123",
		Agent:       "test",
		MinSeverity: "high",
	})
	require.NoError(t, err)
	assert.Equal("high", job.MinSeverity)

	// Claim preserves it
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal("high", claimed.MinSeverity)

	// GetJobByID preserves it
	got, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal("high", got.MinSeverity)

	// ListJobs preserves it
	jobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal("high", jobs[0].MinSeverity)

	// Empty MinSeverity round-trips as empty
	job2, err := db.EnqueueJob(EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "def456",
		Agent:    "test",
	})
	require.NoError(t, err)
	assert.Empty(job2.MinSeverity)
}

func TestMinSeverityNormalizesOnWrite(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/min-sev-norm")
	commit := createCommit(t, db, repo.ID, "abc123")

	// Invalid value gets dropped to empty
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:      repo.ID,
		CommitID:    commit.ID,
		GitRef:      "abc123",
		Agent:       "test",
		MinSeverity: "bogus",
	})
	require.NoError(t, err)
	assert.Empty(job.MinSeverity)
}

func TestReenqueueJob_AcceptsSkipped(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repoID := createRepo(t, db, "/tmp/repo-rerun-skipped").ID
	commitID := createCommit(t, db, repoID, "feed").ID

	res, err := db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, agent, status, review_type, skip_reason)
		VALUES (?, ?, 'feed', 'codex', 'skipped', 'design', 'trivial')
	`, repoID, commitID)
	require.NoError(t, err)
	jobID, err := res.LastInsertId()
	require.NoError(t, err)

	require.NoError(t, db.ReenqueueJob(jobID, ReenqueueOpts{}))

	j, err := db.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusQueued, j.Status)
}

func seedRunningClassify(t *testing.T, db *DB, path, sha, workerID string) int64 {
	t.Helper()
	repo := createRepo(t, db, path)
	commit := createCommit(t, db, repo.ID, sha)
	var jobID int64
	require.NoError(t, db.QueryRow(`
		INSERT INTO review_jobs
		  (repo_id, commit_id, git_ref, status, job_type, review_type, source, worker_id, started_at, enqueued_at, updated_at)
		VALUES (?, ?, ?, 'running', 'classify', 'design', 'auto_design', ?, datetime('now'), datetime('now'), datetime('now'))
		RETURNING id
	`, repo.ID, commit.ID, sha, workerID).Scan(&jobID))
	return jobID
}

func TestPromoteClassifyToDesignReview_HappyPath(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	jobID := seedRunningClassify(t, db, "/tmp/repo-promote", "abc", "w1")

	_, err := db.Exec(`UPDATE review_jobs SET error = ? WHERE id = ?`,
		"classifier retry: timeout", jobID)
	require.NoError(t, err)

	// The classifier attempt left a session id and the agent_invoked marker
	// behind. Both must clear on promotion so the design attempt captures a
	// fresh session (the token-usage write is session-scoped) and is not counted
	// as having run an agent until it actually selects one.
	setJobSession(t, db, jobID, "classify-sess")
	require.NoError(t, db.MarkJobAgentInvoked(jobID, "w1", "codex classify abc"))

	require.NoError(t, db.PromoteClassifyToDesignReview(jobID, "w1", "claude-code", ""))

	j, err := db.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusQueued, j.Status)
	assert.Equal(t, "review", j.JobType)
	assert.Equal(t, "design", j.ReviewType)
	assert.Equal(t, "auto_design", j.Source)
	assert.Empty(t, j.WorkerID, "worker_id cleared so a new worker can claim")
	assert.Nil(t, j.StartedAt, "started_at cleared")
	assert.Empty(t, j.Error, "error cleared")
	assert.Empty(t, j.SessionID, "session id cleared so the design attempt captures a fresh one")
	assert.Empty(t, j.CommandLine, "command line cleared on promotion")
	assert.False(t, getJobAgentInvoked(t, db, jobID),
		"agent_invoked marker cleared so promotion is not counted as an agent run")
}

func TestPromoteClassifyToDesignReview_StaleWorkerNoOps(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	jobID := seedRunningClassify(t, db, "/tmp/repo-promote-stale", "abc", "w1")

	err := db.PromoteClassifyToDesignReview(jobID, "w2", "claude-code", "")
	require.ErrorIs(t, err, sql.ErrNoRows)

	j, err := db.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusRunning, j.Status, "row unchanged by stale worker")
	assert.Equal(t, "classify", j.JobType)
}

func TestPromoteClassifyToDesignReview_CanceledNoOps(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/repo-promote-cancel")
	commit := createCommit(t, db, repo.ID, "abc")
	var jobID int64
	require.NoError(t, db.QueryRow(`
		INSERT INTO review_jobs
		  (repo_id, commit_id, git_ref, status, job_type, review_type, source, worker_id, enqueued_at, updated_at)
		VALUES (?, ?, 'abc', 'canceled', 'classify', 'design', 'auto_design', 'w1', datetime('now'), datetime('now'))
		RETURNING id
	`, repo.ID, commit.ID).Scan(&jobID))

	err := db.PromoteClassifyToDesignReview(jobID, "w1", "claude-code", "")
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestMarkClassifyAsSkippedDesign_HappyPath(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	jobID := seedRunningClassify(t, db, "/tmp/repo-skip", "abc", "w1")
	require.NoError(t, db.MarkClassifyAsSkippedDesign(jobID, "w1", "trivial diff", ""))

	j, err := db.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusSkipped, j.Status)
	assert.Equal(t, "review", j.JobType)
	assert.Equal(t, "trivial diff", j.SkipReason)
	assert.Empty(t, j.Error, "error column stays empty on clean 'no' verdict")
}

func TestMarkClassifyAsSkippedDesign_WritesErrorOnFailure(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	jobID := seedRunningClassify(t, db, "/tmp/repo-skip-err", "abc", "w1")
	require.NoError(t, db.MarkClassifyAsSkippedDesign(
		jobID, "w1", "classifier failed",
		"exec: /nix/store/abc/bin/claude: stderr=timeout after 30s",
	))

	j, err := db.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusSkipped, j.Status)
	assert.Equal(t, "classifier failed", j.SkipReason)
	assert.Contains(t, j.Error, "timeout after 30s",
		"full error text must be persisted to job.error for operator debugging")
}

func TestMarkClassifyAsSkippedDesign_StaleWorkerNoOps(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	jobID := seedRunningClassify(t, db, "/tmp/repo-skip-stale", "abc", "w1")

	err := db.MarkClassifyAsSkippedDesign(jobID, "w-other", "some reason", "")
	require.ErrorIs(t, err, sql.ErrNoRows)

	j, err := db.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusRunning, j.Status)
	assert.Equal(t, "classify", j.JobType)
}

func TestMarkClassifyAsSkippedDesign_CanceledNoOps(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/repo-skip-cancel")
	commit := createCommit(t, db, repo.ID, "abc")
	var jobID int64
	require.NoError(t, db.QueryRow(`
		INSERT INTO review_jobs
		  (repo_id, commit_id, git_ref, status, job_type, review_type, source, worker_id, enqueued_at, updated_at)
		VALUES (?, ?, 'abc', 'canceled', 'classify', 'design', 'auto_design', 'w1', datetime('now'), datetime('now'))
		RETURNING id
	`, repo.ID, commit.ID).Scan(&jobID))

	err := db.MarkClassifyAsSkippedDesign(jobID, "w1", "some reason", "")
	require.ErrorIs(t, err, sql.ErrNoRows)

	j, err := db.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusCanceled, j.Status)
}

func TestInsertSkippedDesignJob_BasicAndDedup(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/repo-skip-insert")
	commit := createCommit(t, db, repo.ID, "abc")

	require.NoError(t, db.InsertSkippedDesignJob(InsertSkippedDesignJobParams{
		RepoID:     repo.ID,
		CommitID:   commit.ID,
		GitRef:     "abc",
		SkipReason: "trivial",
	}))

	// Second insert is a no-op due to dedup index.
	require.NoError(t, db.InsertSkippedDesignJob(InsertSkippedDesignJobParams{
		RepoID:     repo.ID,
		CommitID:   commit.ID,
		GitRef:     "abc",
		SkipReason: "different reason",
	}))

	jobs, err := db.ListJobsByStatus(repo.ID, JobStatusSkipped)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "design", jobs[0].ReviewType)
	assert.Equal(t, "trivial", jobs[0].SkipReason)
	assert.Equal(t, "auto_design", jobs[0].Source)
}

func TestEnqueueAutoDesignJob_BasicAndDedup(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/repo-enq-auto")
	commit := createCommit(t, db, repo.ID, "abc")

	id1, err := db.EnqueueAutoDesignJob(EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   commit.ID,
		GitRef:     "abc",
		JobType:    JobTypeReview,
		ReviewType: "design",
	})
	require.NoError(t, err)
	assert.NotZero(t, id1)

	// Second enqueue is a no-op (returns 0).
	id2, err := db.EnqueueAutoDesignJob(EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   commit.ID,
		GitRef:     "abc",
		JobType:    JobTypeReview,
		ReviewType: "design",
	})
	require.NoError(t, err)
	assert.Zero(t, id2)
}

func TestHasAutoDesignSlotForCommit(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repoID := createRepo(t, db, "/tmp/repo-hasdesign").ID
	createCommit(t, db, repoID, "cafef00d")
	has, err := db.HasAutoDesignSlotForCommit(repoID, "cafef00d")
	require.NoError(t, err)
	assert.False(t, has)

	_, err = db.Exec(
		`INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type, source)
		 VALUES (?, (SELECT id FROM commits WHERE sha=?), ?, 'queued', 'design', 'auto_design')`,
		repoID, "cafef00d", "cafef00d")
	require.NoError(t, err)
	has, err = db.HasAutoDesignSlotForCommit(repoID, "cafef00d")
	require.NoError(t, err)
	assert.True(t, has)

	repoIDExplicit := createRepo(t, db, "/tmp/repo-hasdesign-explicit").ID
	createCommit(t, db, repoIDExplicit, "beef")
	_, err = db.Exec(
		`INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type)
		 VALUES (?, (SELECT id FROM commits WHERE sha=?), ?, 'queued', 'design')`,
		repoIDExplicit, "beef", "beef")
	require.NoError(t, err)
	has, err = db.HasAutoDesignSlotForCommit(repoIDExplicit, "beef")
	require.NoError(t, err)
	assert.False(t, has, "explicit source=NULL design rows must not count as slot-occupied")

	repoIDCls := createRepo(t, db, "/tmp/repo-hasdesign-cls").ID
	createCommit(t, db, repoIDCls, "abc")
	_, err = db.Exec(
		`INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, job_type, review_type, source)
		 VALUES (?, (SELECT id FROM commits WHERE sha=?), ?, 'queued', 'classify', 'design', 'auto_design')`,
		repoIDCls, "abc", "abc")
	require.NoError(t, err)
	has, err = db.HasAutoDesignSlotForCommit(repoIDCls, "abc")
	require.NoError(t, err)
	assert.True(t, has, "queued classify job must count as slot-occupied")

	repoIDSk := createRepo(t, db, "/tmp/repo-hasdesign-skipped").ID
	createCommit(t, db, repoIDSk, "feed")
	_, err = db.Exec(
		`INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type, skip_reason, source)
		 VALUES (?, (SELECT id FROM commits WHERE sha=?), ?, 'skipped', 'design', 'trivial', 'auto_design')`,
		repoIDSk, "feed", "feed")
	require.NoError(t, err)
	has, err = db.HasAutoDesignSlotForCommit(repoIDSk, "feed")
	require.NoError(t, err)
	assert.True(t, has)
}

func TestHasAutoDesignSlotForCommit_CommitlessRowOccupiesByGitRef(t *testing.T) {
	// When commit metadata lookup fails at dispatch time, an
	// auto_design row is inserted with commit_id=NULL. A later
	// dispatch that successfully resolves commit_id must still
	// see the slot as occupied — otherwise the same git_ref ends
	// up with two auto_design rows (the (NULL, ...) row and the
	// new (resolved_id, ...) row, allowed by the partial unique
	// index because NULL != NULL).
	db := openTestDB(t)
	defer db.Close()

	repoID := createRepo(t, db, "/tmp/repo-hasdesign-commitless").ID

	// Sanity: no row, no slot.
	has, err := db.HasAutoDesignSlotForCommit(repoID, "deadbeef")
	require.NoError(t, err)
	assert.False(t, has)

	// First dispatch: commit metadata unavailable, row inserted
	// without commit_id but with git_ref set.
	_, err = db.Exec(
		`INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, job_type, review_type, source)
		 VALUES (?, NULL, ?, 'queued', 'classify', 'design', 'auto_design')`,
		repoID, "deadbeef")
	require.NoError(t, err)

	has, err = db.HasAutoDesignSlotForCommit(repoID, "deadbeef")
	require.NoError(t, err)
	assert.True(t, has,
		"commitless auto_design row must occupy the slot for its git_ref")

	// Different git_ref must not match (no false positive).
	has, err = db.HasAutoDesignSlotForCommit(repoID, "cafe")
	require.NoError(t, err)
	assert.False(t, has)
}

func TestGetJobCounts_IncludesSkipped(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repoID := createRepo(t, db, "/tmp/repo-counts").ID
	commitID := createCommit(t, db, repoID, "abc").ID

	_, err := db.Exec(`
		INSERT INTO review_jobs (repo_id, commit_id, git_ref, status, review_type, skip_reason)
		VALUES (?, ?, 'abc', 'skipped', 'design', 'trivial')
	`, repoID, commitID)
	require.NoError(t, err)

	queued, running, done, failed, canceled, applied, rebased, skipped, err := db.GetJobCounts()
	require.NoError(t, err)
	assert.Equal(t, 0, queued)
	assert.Equal(t, 0, running)
	assert.Equal(t, 0, done)
	assert.Equal(t, 0, failed)
	assert.Equal(t, 0, canceled)
	assert.Equal(t, 0, applied)
	assert.Equal(t, 0, rebased)
	assert.Equal(t, 1, skipped)
}

func TestBackupColumnsRoundTrip(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/backup-rt")
	commit := createCommit(t, db, repo.ID, "abc123")

	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:      repo.ID,
		CommitID:    commit.ID,
		GitRef:      "abc123",
		Agent:       "test",
		JobType:     JobTypeReview,
		BackupAgent: "claude-code",
		BackupModel: "opus",
	})
	require.NoError(t, err)
	assert.Equal("claude-code", job.BackupAgent)
	assert.Equal("opus", job.BackupModel)

	got, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal("claude-code", got.BackupAgent)
	assert.Equal("opus", got.BackupModel)

	// ListJobs and ClaimJob build the same full ReviewJob and must round-trip too.
	jobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal("claude-code", jobs[0].BackupAgent)
	assert.Equal("opus", jobs[0].BackupModel)

	claimed := claimJob(t, db, "worker-1")
	assert.Equal("claude-code", claimed.BackupAgent)
	assert.Equal("opus", claimed.BackupModel)
}

func TestGetJobsToSyncIncludesBackupColumns(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	machineID, err := db.GetMachineID()
	require.NoError(t, err)

	repo := createRepo(t, db, "/tmp/backup-sync")
	commit := createCommit(t, db, repo.ID, "abc123")
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", Agent: "test",
		JobType: JobTypeReview, BackupAgent: "gemini", BackupModel: "gemini-2.5-pro",
	})
	require.NoError(t, err)
	setStatus(t, db, job.ID, JobStatusDone)

	jobs, err := db.GetJobsToSync(machineID, 100)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal("gemini", jobs[0].BackupAgent)
	assert.Equal("gemini-2.5-pro", jobs[0].BackupModel)
}

func TestUpsertPulledJobRoundTripsBackupColumns(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/backup-pull")

	pulled := PulledJob{
		UUID:            "backup-uuid-1",
		GitRef:          "abc123",
		Agent:           "test",
		Reasoning:       "thorough",
		JobType:         JobTypeReview,
		Status:          "done",
		EnqueuedAt:      time.Now(),
		UpdatedAt:       time.Now(),
		SourceMachineID: "remote-machine",
		BackupAgent:     "copilot",
		BackupModel:     "gpt-5",
	}
	require.NoError(t, db.UpsertPulledJob(pulled, repo.ID, nil))

	var ba, bm string
	row := db.QueryRow(
		`SELECT COALESCE(backup_agent,''), COALESCE(backup_model,'') FROM review_jobs WHERE uuid = 'backup-uuid-1'`,
	)
	require.NoError(t, row.Scan(&ba, &bm))
	assert.Equal("copilot", ba)
	assert.Equal("gpt-5", bm)
}

func TestClaimJobHydratesUUID(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	_, _, enqueued := createJobChain(t, db, "/repo/uuid-hydration", "abc123")
	require.NotEmpty(t, enqueued.UUID, "enqueue must assign a job UUID")

	claimed := claimJob(t, db, "worker-1")
	assert.Equal(t, enqueued.ID, claimed.ID)
	assert.Equal(t, enqueued.UUID, claimed.UUID,
		"claimed job must carry the stored UUID so completion events keep UUID-based hook idempotency")
}
