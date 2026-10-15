package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBudgetSpendCompletionDay(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/budget-repo")
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for i, tc := range []struct{ finished, usage string }{
		{"2026-01-02T00:00:00Z", `{"has_cost":true,"cost_usd":1.25}`},
		{"2026-01-02 23:59:59", `{"has_cost":true,"cost_usd":0}`},
		{"2026-01-01T23:59:59Z", `{"has_cost":true,"cost_usd":9}`},
		{"2026-01-03T00:00:00Z", `{"has_cost":true,"cost_usd":9}`},
		{"2026-01-02T12:00:00Z", `{"has_cost":true}`},
		{"2026-01-02T12:00:00Z", `{"has_cost":true,"cost_usd":-1}`},
		{"2026-01-02T12:00:00Z", `{"has_cost":true,"cost_usd":"5"}`},
		{"2026-01-02T12:00:00Z", `broken`},
		{"2026-01-02T12:00:00Z", `{"has_cost":false,"cost_usd":5}`},
	} {
		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "HEAD", Agent: "test"})
		require.NoError(t, err)
		_, err = db.Exec(`UPDATE review_jobs SET status='done',enqueued_at='2026-01-01T12:00:00Z', started_at='2026-01-01T23:00:00Z',finished_at=?,agent_invoked=1,token_usage=? WHERE id=?`, tc.finished, tc.usage, job.ID)
		require.NoError(t, err, "case %d", i)
	}
	got, err := db.GetBudgetSpend(day)
	require.NoError(t, err)
	assert.InDelta(t, 1.25, got.TotalUSD, 0.000001)
	assert.Equal(t, 2, got.JobsWithCost)
}

func TestBudgetSelectionOwnershipAndLifecycle(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/budget-routing-repo")
	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "HEAD", Agent: "claude-code", Model: "primary-model", Provider: "primary-provider"})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("worker-a")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	ok, err := db.SetJobBudgetAgent(job.ID, "other-worker", "test", "", "", "")
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = db.SetJobBudgetAgent(job.ID, "worker-a", "codex", "", "gemini", "fallback-model")
	require.NoError(t, err)
	require.True(t, ok)
	got, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "codex", got.Agent)
	assert.Empty(t, got.Model)
	assert.Empty(t, got.Provider)
	assert.Equal(t, "gemini", got.BackupAgent)
	assert.Equal(t, "fallback-model", got.BackupModel)
	assert.True(t, got.BudgetRoutingLocked)
	ok, err = db.FailoverJob(job.ID, "worker-a", "test", "")
	require.NoError(t, err)
	require.True(t, ok)
	claimed, err = db.ClaimJob("worker-b")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.True(t, claimed.BudgetRoutingLocked)
	ok, err = db.RetryJob(job.ID, "worker-b", 3, 0)
	require.NoError(t, err)
	require.True(t, ok)
	claimed, err = db.ClaimJob("worker-c")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.True(t, claimed.BudgetRoutingLocked)
	require.NoError(t, db.CompleteJob(job.ID, "test", `{"schema_version":2,"summary":"No issues found.","verdict":"pass","findings":[]}`))
	require.NoError(t, db.ReenqueueJob(job.ID, ReenqueueOpts{}))
	claimed, err = db.ClaimJob("worker-d")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.False(t, claimed.BudgetRoutingLocked)
	assert.Equal(t, "claude-code", claimed.Agent)
	assert.Empty(t, claimed.BackupAgent)
	assert.Empty(t, claimed.BackupModel)
}

func TestBudgetFailoverLocksUnroutedJob(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/budget-failover-repo")
	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "HEAD", Agent: "codex"})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("worker-a")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.False(t, claimed.BudgetRoutingLocked)
	ok, err := db.FailoverJob(job.ID, "worker-a", "test", "")
	require.NoError(t, err)
	require.True(t, ok)
	claimed, err = db.ClaimJob("worker-b")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.True(t, claimed.BudgetRoutingLocked)
	assert.Zero(t, claimed.RetryCount)
	err = db.CancelJob(job.ID)
	require.NoError(t, err)
	ok, err = db.SetJobBudgetAgent(job.ID, "worker-b", "codex", "", "", "")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestBudgetSpendRerunReplacesPriorCost(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/budget-rerun-repo")
	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "HEAD", Agent: "test"})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET status='done',started_at=datetime('now'),finished_at=datetime('now'),agent_invoked=1,token_usage='{"has_cost":true,"cost_usd":5}' WHERE id=?`, job.ID)
	require.NoError(t, err)
	spend, err := db.GetBudgetSpend(time.Now())
	require.NoError(t, err)
	assert.InDelta(t, 5, spend.TotalUSD, 0.00001)
	require.NoError(t, db.ReenqueueJob(job.ID, ReenqueueOpts{}))
	spend, err = db.GetBudgetSpend(time.Now())
	require.NoError(t, err)
	assert.Zero(t, spend.TotalUSD)
	assert.Zero(t, spend.JobsWithCost)
}

func TestBudgetFailoverLockSurvivesRestart(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	var seq int
	var name, path string
	require.NoError(t, db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path))
	repo := createRepo(t, db, "/tmp/budget-restart-repo")
	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "HEAD", Agent: "codex"})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("worker-a")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	changed, err := db.FailoverJob(job.ID, "worker-a", "test", "")
	require.NoError(t, err)
	require.True(t, changed)
	claimed, err = db.ClaimJob("worker-b")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.NoError(t, db.Close())
	db, err = Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, db.ResetStaleJobs())
	claimed, err = db.ClaimJob("worker-c")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.True(t, claimed.BudgetRoutingLocked)
	assert.Equal(t, "test", claimed.Agent)
	assert.Zero(t, claimed.RetryCount)
}

func TestBudgetSpendIncludesPaidSkippedAttempts(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/budget-skipped-repo")
	for _, usage := range []string{`{"has_cost":true,"cost_usd":0.25}`, ""} {
		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "HEAD", Agent: "test"})
		require.NoError(t, err)
		_, err = db.Exec(`UPDATE review_jobs SET status='skipped',started_at=datetime('now'),finished_at=datetime('now'),agent_invoked=?,token_usage=? WHERE id=?`, usage != "", usage, job.ID)
		require.NoError(t, err)
	}
	spend, err := db.GetBudgetSpend(time.Now())
	require.NoError(t, err)
	assert.InDelta(t, 0.25, spend.TotalUSD, 1e-9)
	assert.Equal(t, 1, spend.JobsWithCost)
	assert.Equal(t, 1, spend.JobsTotal)
}

func TestBudgetSpendCountsHistoricalUnpricedInvocations(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/budget-historical-repo")
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, usage := range []string{
		`{"has_cost":true,"cost_usd":0.25}`,
		`{"has_cost":true}`,
		`{"cache_creation_tokens":8192}`,
		"", `{}`, `broken`,
	} {
		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "HEAD", Agent: "test"})
		require.NoError(t, err)
		_, err = db.Exec(`UPDATE review_jobs SET status='done',started_at='2026-01-02T10:00:00Z',
			finished_at='2026-01-02T11:00:00Z',agent_invoked=0,token_usage=? WHERE id=?`, usage, job.ID)
		require.NoError(t, err)
	}
	spend, err := db.GetBudgetSpend(day)
	require.NoError(t, err)
	assert.Equal(3, spend.JobsTotal, "usage proves invocation even when the marker is absent")
	assert.Equal(1, spend.JobsWithCost)
	assert.InDelta(0.25, spend.TotalUSD, 1e-9)
	assert.False(spend.Complete, "unknown costs leave coverage incomplete")
}
