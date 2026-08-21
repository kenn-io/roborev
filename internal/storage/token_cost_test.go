package storage

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrationMakesLegacyLocalJobEligibleForTokenReconciliation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reviews.db")
	db, err := Open(dbPath)
	require.NoError(t, err)
	_, jobs := seedJobs(t, db, filepath.Join(t.TempDir(), "repo"), 1)
	_, err = db.Exec(`
		UPDATE review_jobs
		SET status = 'done', started_at = datetime('now'),
		    finished_at = datetime('now'), session_id = 'legacy-session',
		    agent_invoked = 1, token_usage = '', source_machine_id = NULL
		WHERE id = ?`, jobs[0].ID)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	machineID, err := db.GetMachineID()
	require.NoError(t, err)
	var sourceMachineID string
	require.NoError(t, db.QueryRow(
		`SELECT source_machine_id FROM review_jobs WHERE id = ?`, jobs[0].ID,
	).Scan(&sourceMachineID))
	assert.Equal(t, machineID, sourceMachineID)
	var historyCount int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM review_job_session_history
		WHERE source_machine_id = ? AND session_id = 'legacy-session'`,
		machineID,
	).Scan(&historyCount))
	assert.Equal(t, 1, historyCount)

	candidates, err := db.ListTokenCostCandidates(0, 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, jobs[0].ID, candidates[0].JobID)
}

func TestListTokenCostCandidatesSelectsRecoverableTerminalJobs(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/token-cost-candidates", 14)

	seeds := []struct {
		status       JobStatus
		sessionID    string
		agentInvoked bool
		tokenUsage   string
	}{
		{JobStatusDone, "done-session", true, `{}`},
		{JobStatusFailed, "failed-session", true, `{}`},
		{JobStatusCanceled, "canceled-session", true, `{}`},
		{JobStatusSkipped, "skipped-session", true, `{}`},
		{JobStatusApplied, "applied-session", true, `{}`},
		{JobStatusRebased, "rebased-session", true, `{}`},
		{JobStatusDone, "usage-evidence-session", false, `{"total_output_tokens":42}`},
		{JobStatusDone, "priced-session", true, `{"has_cost":true,"cost_usd":0.25}`},
		{JobStatusDone, "no-agent-session", false, `{}`},
		{JobStatusRunning, "running-session", true, `{}`},
		{JobStatusDone, "", true, `{}`},
		{JobStatusDone, "reused-session", true, `{}`},
		{JobStatusFailed, "reused-session", true, `{}`},
		{JobStatusQueued, "queued-session", true, `{}`},
	}
	for i, seed := range seeds {
		invoked := 0
		if seed.agentInvoked {
			invoked = 1
		}
		_, err := db.Exec(`
			UPDATE review_jobs
			SET status = ?, started_at = datetime('now'),
			    finished_at = CASE WHEN ? IN ('queued', 'running') THEN NULL ELSE datetime('now') END,
			    session_id = ?, agent_invoked = ?, token_usage = ?
			WHERE id = ?`,
			seed.status, seed.status, seed.sessionID, invoked, seed.tokenUsage, jobs[i].ID)
		require.NoError(t, err)
	}

	got, err := db.ListTokenCostCandidates(0, 100)
	require.NoError(t, err)

	assert := assert.New(t)
	require.Len(t, got, 7)
	assert.Equal(jobs[0].ID, got[0].JobID)
	assert.Equal(jobs[1].ID, got[1].JobID)
	assert.Equal(jobs[2].ID, got[2].JobID)
	assert.Equal(jobs[3].ID, got[3].JobID)
	assert.Equal(jobs[4].ID, got[4].JobID)
	assert.Equal(jobs[5].ID, got[5].JobID)
	assert.Equal(jobs[6].ID, got[6].JobID)
	assert.Equal("usage-evidence-session", got[6].SessionID)
	assert.Equal(`{"total_output_tokens":42}`, got[6].TokenUsage)
}

func TestListTokenCostCandidatesPagesByExclusiveJobID(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/token-cost-pages", 4)
	for i, job := range jobs {
		_, err := db.Exec(`
			UPDATE review_jobs
			SET status = 'done', started_at = datetime('now'), finished_at = datetime('now'),
			    session_id = ?, agent_invoked = 1, token_usage = '{}'
			WHERE id = ?`,
			"page-session-"+string(rune('a'+i)), job.ID)
		require.NoError(t, err)
	}

	first, err := db.ListTokenCostCandidates(0, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	assert.Equal(t, jobs[0].ID, first[0].JobID)
	assert.Equal(t, jobs[1].ID, first[1].JobID)

	second, err := db.ListTokenCostCandidates(first[1].JobID, 2)
	require.NoError(t, err)
	require.Len(t, second, 2)
	assert.Equal(t, jobs[2].ID, second[0].JobID)
	assert.Equal(t, jobs[3].ID, second[1].JobID)

	last, err := db.ListTokenCostCandidates(second[1].JobID, 2)
	require.NoError(t, err)
	assert.Empty(t, last)

	got, err := db.GetTokenCostCandidate(jobs[2].ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, jobs[2].ID, got.JobID)

	_, err = db.Exec(
		`UPDATE review_jobs SET token_usage = '{"has_cost":true,"cost_usd":0}' WHERE id = ?`,
		jobs[2].ID)
	require.NoError(t, err)
	got, err = db.GetTokenCostCandidate(jobs[2].ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestListTokenCostCandidatesIncludesSerializedUnpricedUsage(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/token-cost-unpriced-json", 1)
	_, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'done', started_at = datetime('now'),
		    finished_at = datetime('now'), session_id = 'unpriced-session',
		    agent_invoked = 1,
		    token_usage = '{"total_output_tokens":42,"cost_usd":0}'
		WHERE id = ?`, jobs[0].ID)
	require.NoError(t, err)

	got, err := db.ListTokenCostCandidates(0, 100)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, jobs[0].ID, got[0].JobID)
}

func TestListTokenUsageLogCandidatesSelectsMissingSession(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/token-log-candidates", 3)
	tokenUsage := []string{"", "", `{"has_cost":true,"cost_usd":0.25}`}
	for i, sessionID := range []string{"", "stored-session", ""} {
		_, err := db.Exec(`
			UPDATE review_jobs
			SET status = 'done', started_at = datetime('now'),
			    finished_at = datetime('now'), session_id = ?,
			    agent_invoked = 1, token_usage = ?
			WHERE id = ?`,
			sessionID,
			tokenUsage[i],
			jobs[i].ID,
		)
		require.NoError(t, err)
	}

	got, err := db.ListTokenUsageLogCandidates(0, 100)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, jobs[0].ID, got[0].JobID)
	assert.False(t, got[0].StartedAt.IsZero())
}

func TestTokenReconciliationWaitsForCanceledWorkerRelease(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/token-cost-canceled-release", 2)

	costJob, err := db.ClaimJob("cost-worker")
	require.NoError(t, err)
	require.Equal(t, jobs[0].ID, costJob.ID)
	require.NoError(t, db.MarkJobAgentInvoked(
		costJob.ID, "cost-worker", "codex review",
	))
	require.NoError(t, db.SaveJobSessionID(
		costJob.ID, "cost-worker", "canceled-cost-session",
	))
	require.NoError(t, db.CancelJob(costJob.ID))

	logJob, err := db.ClaimJob("log-worker")
	require.NoError(t, err)
	require.Equal(t, jobs[1].ID, logJob.ID)
	require.NoError(t, db.MarkJobAgentInvoked(
		logJob.ID, "log-worker", "codex review",
	))
	require.NoError(t, db.CancelJob(logJob.ID))

	costCandidates, err := db.ListTokenCostCandidates(0, 10)
	require.NoError(t, err)
	assert.Empty(t, costCandidates)
	logCandidates, err := db.ListTokenUsageLogCandidates(0, 10)
	require.NoError(t, err)
	assert.Empty(t, logCandidates)

	released, err := db.ReleaseCanceledJob(costJob.ID, "cost-worker")
	require.NoError(t, err)
	require.True(t, released)
	released, err = db.ReleaseCanceledJob(logJob.ID, "log-worker")
	require.NoError(t, err)
	require.True(t, released)

	costCandidates, err = db.ListTokenCostCandidates(0, 10)
	require.NoError(t, err)
	require.Len(t, costCandidates, 1)
	assert.Equal(t, costJob.ID, costCandidates[0].JobID)
	logCandidates, err = db.ListTokenUsageLogCandidates(0, 10)
	require.NoError(t, err)
	require.Len(t, logCandidates, 1)
	assert.Equal(t, logJob.ID, logCandidates[0].JobID)
}

func TestTokenUsageCandidatesExcludeImportedJobs(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/token-cost-ownership", 4)
	machineID, err := db.GetMachineID()
	require.NoError(t, err)

	for i, seed := range []struct {
		sessionID       string
		sourceMachineID string
	}{
		{sessionID: "local-cost-session", sourceMachineID: machineID},
		{sessionID: "remote-cost-session", sourceMachineID: "remote-machine"},
		{sourceMachineID: machineID},
		{sourceMachineID: "remote-machine"},
	} {
		_, err := db.Exec(`
			UPDATE review_jobs
			SET status = 'done', started_at = datetime('now'), finished_at = datetime('now'),
			    session_id = ?, agent_invoked = 1, token_usage = '',
			    source_machine_id = ?, synced_at = datetime('now')
			WHERE id = ?`, seed.sessionID, seed.sourceMachineID, jobs[i].ID)
		require.NoError(t, err)
	}

	costCandidates, err := db.ListTokenCostCandidates(0, 100)
	require.NoError(t, err)
	require.Len(t, costCandidates, 1)
	assert.Equal(t, jobs[0].ID, costCandidates[0].JobID)

	logCandidates, err := db.ListTokenUsageLogCandidates(0, 100)
	require.NoError(t, err)
	require.Len(t, logCandidates, 1)
	assert.Equal(t, jobs[2].ID, logCandidates[0].JobID)

	updated, err := db.BackfillJobTokenUsageIfCurrent(
		jobs[1].ID,
		"remote-cost-session",
		"",
		`{"has_cost":true,"cost_usd":0.25}`,
		"",
		true,
	)
	require.NoError(t, err)
	assert.False(t, updated)

	remote, err := db.GetJobByID(jobs[1].ID)
	require.NoError(t, err)
	assert.Empty(t, remote.TokenUsage)
	var syncedAt sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT synced_at FROM review_jobs WHERE id = ?`, jobs[1].ID,
	).Scan(&syncedAt))
	assert.True(t, syncedAt.Valid)
}

func TestTokenCostCandidateRejectsSessionReusedByPriorAttempt(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/token-cost-attempt-history", 1)

	first, err := db.ClaimJob("first-worker")
	require.NoError(t, err)
	require.Equal(t, jobs[0].ID, first.ID)
	require.NoError(t, db.MarkJobAgentInvoked(first.ID, "first-worker", "codex review"))
	require.NoError(t, db.SaveJobSessionID(first.ID, "first-worker", "reused-session"))
	retried, err := db.RetryJob(first.ID, "first-worker", 3, 0)
	require.NoError(t, err)
	require.True(t, retried)

	second, err := db.ClaimJob("second-worker")
	require.NoError(t, err)
	require.Equal(t, jobs[0].ID, second.ID)
	require.NoError(t, db.MarkJobAgentInvoked(second.ID, "second-worker", "codex review"))
	require.NoError(t, db.SaveJobSessionID(second.ID, "second-worker", "reused-session"))
	require.NoError(t, db.CompleteJob(
		second.ID, "codex", "prompt", "No issues found.",
	))

	candidates, err := db.ListTokenCostCandidates(0, 100)
	require.NoError(t, err)
	assert.Empty(t, candidates)

	updated, err := db.BackfillJobTokenUsageIfCurrent(
		second.ID,
		"reused-session",
		"",
		`{"has_cost":true,"cost_usd":0.25}`,
		second.StartedAtRaw,
		true,
	)
	require.NoError(t, err)
	assert.False(t, updated)
}

func TestTokenCostCandidateRejectsPrepopulatedSessionReusedByLaterAttempt(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/token-cost-prepopulated-session")
	commit := createCommit(t, db, repo.ID, "prepopulated-session-sha")
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:    repo.ID,
		CommitID:  commit.ID,
		GitRef:    "prepopulated-session-sha",
		Agent:     "codex",
		SessionID: "reused-session",
	})
	require.NoError(t, err)

	first, err := db.ClaimJob("first-worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, first.ID)
	require.NoError(t, db.MarkJobAgentInvoked(first.ID, "first-worker", "codex review"))
	retried, err := db.RetryJob(first.ID, "first-worker", 3, 0)
	require.NoError(t, err)
	require.True(t, retried)

	second, err := db.ClaimJob("second-worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, second.ID)
	require.NoError(t, db.MarkJobAgentInvoked(second.ID, "second-worker", "codex review"))
	require.NoError(t, db.SaveJobSessionID(second.ID, "second-worker", "reused-session"))
	require.NoError(t, db.CompleteJob(
		second.ID, "codex", "prompt", "No issues found.",
	))

	candidates, err := db.ListTokenCostCandidates(0, 100)
	require.NoError(t, err)
	assert.Empty(t, candidates)
}

func TestTokenCostCandidateRejectsSessionResumedFromImportedJob(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/token-cost-cross-machine-resume")
	commit := createCommit(t, db, repo.ID, "cross-machine-resume-sha")

	imported, err := db.EnqueueJob(EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "imported-session-source",
		Agent:    "codex",
	})
	require.NoError(t, err)
	_, err = db.Exec(`
		UPDATE review_jobs
		SET status = 'done', started_at = datetime('now', '-2 minutes'),
		    finished_at = datetime('now', '-1 minute'),
		    session_id = 'cross-machine-session', agent_invoked = 1,
		    source_machine_id = 'remote-machine'
		WHERE id = ?`, imported.ID)
	require.NoError(t, err)

	resumed, err := db.EnqueueJob(EnqueueOpts{
		RepoID:    repo.ID,
		CommitID:  commit.ID,
		GitRef:    "cross-machine-resume-sha",
		Agent:     "codex",
		SessionID: "cross-machine-session",
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("resumed-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, resumed.ID, claimed.ID)
	require.NoError(t, db.MarkJobAgentInvoked(
		resumed.ID, "resumed-worker", "codex review",
	))
	require.NoError(t, db.CompleteJob(
		resumed.ID, "codex", "prompt", "No issues found.",
	))

	var sessionResumed int
	require.NoError(t, db.QueryRow(
		`SELECT session_resumed FROM review_jobs WHERE id = ?`, resumed.ID,
	).Scan(&sessionResumed))
	assert.Equal(t, 1, sessionResumed)

	candidates, err := db.ListTokenCostCandidates(0, 100)
	require.NoError(t, err)
	assert.Empty(t, candidates)
}

func TestAutoDesignJobCapturesTokenUsageBeforeReopen(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/token-cost-auto-design")
	commit := createCommit(t, db, repo.ID, "auto-design-sha")
	jobID, err := db.EnqueueAutoDesignJob(EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   commit.ID,
		GitRef:     "auto-design-sha",
		Agent:      "codex",
		JobType:    JobTypeReview,
		ReviewType: "design",
	})
	require.NoError(t, err)
	require.NotZero(t, jobID)

	claimed, err := db.ClaimJob("auto-design-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, jobID, claimed.ID)
	require.NoError(t, db.MarkJobAgentInvoked(
		jobID, "auto-design-worker", "codex review",
	))
	require.NoError(t, db.SaveJobSessionID(
		jobID, "auto-design-worker", "auto-design-session",
	))
	require.NoError(t, db.CompleteJob(
		jobID, "codex", "prompt", "No issues found.",
	))

	updated, err := db.BackfillJobTokenUsageIfCurrent(
		jobID,
		"auto-design-session",
		"",
		`{"total_output_tokens":32}`,
		claimed.StartedAtRaw,
		false,
	)
	require.NoError(t, err)
	assert.True(t, updated)
}

func TestLocallyRerunImportedJobCapturesAndSyncsTokenUsage(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/token-cost-imported-rerun", 1)
	job := jobs[0]
	_, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'done', started_at = datetime('now'),
		    finished_at = datetime('now'), source_machine_id = 'remote-machine',
		    synced_at = datetime('now')
		WHERE id = ?`, job.ID)
	require.NoError(t, err)

	require.NoError(t, db.ReenqueueJob(job.ID, ReenqueueOpts{}))
	machineID, err := db.GetMachineID()
	require.NoError(t, err)
	var owner string
	require.NoError(t, db.QueryRow(
		`SELECT source_machine_id FROM review_jobs WHERE id = ?`, job.ID,
	).Scan(&owner))
	assert.Equal(t, machineID, owner)
	rerun, err := db.ClaimJob("local-worker")
	require.NoError(t, err)
	require.NotNil(t, rerun)
	require.Equal(t, job.ID, rerun.ID)
	require.NoError(t, db.MarkJobAgentInvoked(
		job.ID, "local-worker", "codex review",
	))
	require.NoError(t, db.SaveJobSessionID(
		job.ID, "local-worker", "local-rerun-session",
	))
	require.NoError(t, db.CompleteJob(
		job.ID, "codex", "prompt", "No issues found.",
	))

	updated, err := db.BackfillJobTokenUsageIfCurrent(
		job.ID,
		"local-rerun-session",
		"",
		`{"total_output_tokens":64,"has_cost":true,"cost_usd":0.02}`,
		rerun.StartedAtRaw,
		false,
	)
	require.NoError(t, err)
	assert.True(t, updated)

	toSync, err := db.GetJobsToSync(machineID, 10)
	require.NoError(t, err)
	require.Len(t, toSync, 1)
	assert.Equal(t, job.ID, toSync[0].ID)
	assert.Contains(t, toSync[0].TokenUsage, `"cost_usd":0.02`)
}

func TestBackfillJobTokenUsageIfCurrentRejectsReenqueuedJob(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/token-cost-attempt-race", 1)
	_, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'done', started_at = '2026-08-20T16:00:00.123456789Z',
		    finished_at = '2026-08-20T16:00:01Z', agent_invoked = 1
		WHERE id = ?`, jobs[0].ID)
	require.NoError(t, err)
	candidates, err := db.ListTokenUsageLogCandidates(0, 10)
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	_, err = db.Exec(`
		UPDATE review_jobs
		SET started_at = '2026-08-20T16:00:00.987654321Z',
		    finished_at = '2026-08-20T16:00:02Z'
		WHERE id = ?`, jobs[0].ID)
	require.NoError(t, err)
	updated, err := db.BackfillJobTokenUsageIfCurrent(
		jobs[0].ID,
		"prior-session",
		candidates[0].TokenUsage,
		`{"total_output_tokens":32,"thread_id":"prior-session"}`,
		candidates[0].StartedAtRaw,
		false,
	)
	require.NoError(t, err)
	assert.False(t, updated)

	job, err := db.GetJobByID(jobs[0].ID)
	require.NoError(t, err)
	assert.Empty(t, job.SessionID)
	assert.Empty(t, job.TokenUsage)
}

func TestBackfillJobTokenUsageIfCurrentRejectsNewSessionReuse(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/token-cost-reuse-race", 2)
	_, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'done', started_at = datetime('now'), finished_at = datetime('now'),
		    session_id = 'shared-session', agent_invoked = 1,
		    token_usage = '{"total_output_tokens":10}'
		WHERE id = ?`, jobs[0].ID)
	require.NoError(t, err)
	candidate, err := db.GetTokenCostCandidate(jobs[0].ID)
	require.NoError(t, err)
	require.NotNil(t, candidate)

	_, err = db.Exec(`
		UPDATE review_jobs
		SET status = 'running', started_at = datetime('now'),
		    session_id = 'shared-session', agent_invoked = 1
		WHERE id = ?`, jobs[1].ID)
	require.NoError(t, err)

	updated, err := db.BackfillJobTokenUsageIfCurrent(
		jobs[0].ID,
		candidate.SessionID,
		candidate.TokenUsage,
		`{"total_output_tokens":10,"has_cost":true,"cost_usd":0.25}`,
		"",
		true,
	)
	require.NoError(t, err)
	assert.False(t, updated)

	job, err := db.GetJobByID(jobs[0].ID)
	require.NoError(t, err)
	assert.Equal(t, `{"total_output_tokens":10}`, job.TokenUsage)
}

func TestBackfillJobTokenUsageIfCurrentRejectsStaleUsage(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/token-cost-stale-race", 1)
	_, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'done', started_at = datetime('now'), finished_at = datetime('now'),
		    session_id = 'stale-session', agent_invoked = 1,
		    token_usage = '{"total_output_tokens":10}'
		WHERE id = ?`, jobs[0].ID)
	require.NoError(t, err)
	candidate, err := db.GetTokenCostCandidate(jobs[0].ID)
	require.NoError(t, err)
	require.NotNil(t, candidate)

	_, err = db.Exec(
		`UPDATE review_jobs SET token_usage = '{"total_output_tokens":20}' WHERE id = ?`,
		jobs[0].ID,
	)
	require.NoError(t, err)
	updated, err := db.BackfillJobTokenUsageIfCurrent(
		jobs[0].ID,
		candidate.SessionID,
		candidate.TokenUsage,
		`{"total_output_tokens":10,"has_cost":true,"cost_usd":0.25}`,
		"",
		true,
	)
	require.NoError(t, err)
	assert.False(t, updated)
}
