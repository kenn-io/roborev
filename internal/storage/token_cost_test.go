package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		true,
	)
	require.NoError(t, err)
	assert.False(t, updated)
}
