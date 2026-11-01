package storage

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ciSummarySince = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

func summaryPanel(t *testing.T, db *DB, repo string, pr int, key string, synthesis any) int64 {
	t.Helper()
	result, err := db.ExecContext(t.Context(), `INSERT INTO ci_pr_panels (github_repo, pr_number, head_sha, panel_run_uuid, synthesis_job_id, created_at) VALUES (?, ?, ?, ?, ?, '2026-01-03T00:00:00Z')`, repo, pr, key, testUUID(key), synthesis)
	require.NoError(t, err)
	id, err := result.LastInsertId()
	require.NoError(t, err)
	return id
}

func TestGetSummary_CI(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	s, err := db.GetSummary(SummaryOptions{Since: ciSummarySince})
	require.NoError(t, err)
	assert.Nil(t, s.CI)
	reasons := []string{"closed_pr", "superseded", "stale_head", "deferred_retry", "repo_mismatch", "canceled", ""}
	for i, reason := range reasons {
		id := summaryPanel(t, db, "acme/api", 7, fmt.Sprint("retired", i), nil)
		_, err := db.ExecContext(t.Context(), `UPDATE ci_pr_panels SET retired_at='2026-01-04', retire_reason=NULLIF(?, '') WHERE id=?`, reason, id)
		require.NoError(t, err)
	}
	id := summaryPanel(t, db, "acme/api", 7, "posted", nil)
	_, err = db.ExecContext(t.Context(), `UPDATE ci_pr_panels SET retired_at='2026-01-04', posted_at='2026-01-03 00:02:00', first_attempt_at='2026-01-03 00:00:00', outcome='review_posted' WHERE id=?`, id)
	require.NoError(t, err)
	// Same PR number in another repository is a separate PR.
	summaryPanel(t, db, "acme/web", 7, "active", nil)
	old := summaryPanel(t, db, "acme/old", 9, "old", nil)
	_, err = db.ExecContext(t.Context(), `UPDATE ci_pr_panels SET created_at='2026-01-01' WHERE id=?`, old)
	require.NoError(t, err)
	s, err = db.GetSummary(SummaryOptions{Since: ciSummarySince})
	require.NoError(t, err)
	require.NotNil(t, s.CI)
	a := assert.New(t)
	a.Equal(9, s.CI.TotalPanels)
	a.Equal(1, s.CI.ActivePanels)
	a.Equal(1, s.CI.PostedPanels)
	a.Equal(7, s.CI.RetiredUnpostedPanels)
	a.Equal(1, s.CI.ClosedBeforePost)
	a.Equal(map[string]int{"closed_pr": 1, "superseded": 1, "stale_head": 1, "deferred_retry": 1, "repo_mismatch": 1, "canceled": 1, "unknown": 2}, s.CI.RetiredByReason)
	a.Equal(map[string]int{"review_posted": 1}, s.CI.Outcomes)
	a.Equal(2, s.CI.DistinctPRs)
	a.InDelta(4.5, s.CI.RunsPerPRP50, 0)
	a.InDelta(7.3, s.CI.RunsPerPRP90, 1e-9)
	a.Equal(8, s.CI.RunsPerPRMax)
	require.Len(t, s.CI.HotPRs, 1)
	a.Equal("acme/api", s.CI.HotPRs[0].GithubRepo)
	a.Equal(8, s.CI.HotPRs[0].Runs)
	a.Equal(1, s.CI.TurnaroundSamples)
	a.InDelta(120, s.CI.TurnaroundP50Secs, 0.001)
	a.Equal(CostAggregate{}, s.CI.Cost)
}

func TestGetSummary_CIFiltersAndCost(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, "/tmp/ci-summary")
	other := createRepo(t, db, "/tmp/ci-summary-other")
	addJob := func(repoID int64, branch, key, status, usage string, invoked bool) int64 {
		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repoID, GitRef: key, Agent: "test", Branch: branch})
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET panel_run_uuid=?, status=?, token_usage=?, agent_invoked=?, started_at='2025-12-01', finished_at='2025-12-02', enqueued_at='2025-12-01' WHERE id=?`, testUUID(key), status, usage, invoked, job.ID)
		require.NoError(t, err)
		return job.ID
	}
	syn := addJob(repo.ID, "feature", "cost", "done", "", false) // passthrough
	id := summaryPanel(t, db, "acme/api", 3, "cost", syn)
	_, err := db.ExecContext(t.Context(), `UPDATE ci_pr_panels SET retired_at='2026-01-04', retire_reason='closed_pr' WHERE id=?`, id)
	require.NoError(t, err)
	addJob(repo.ID, "feature", "cost", "done", `{"has_cost":true,"cost_usd":2}`, true)
	addJob(other.ID, "other-branch", "cost", "canceled", `{"has_cost":true,"cost_usd":3}`, true)
	addJob(repo.ID, "feature", "cost", "failed", `{"has_cost":true,"cost_usd":0}`, true)
	addJob(repo.ID, "feature", "cost", "done", `{"has_cost":true}`, true)
	addJob(repo.ID, "feature", "cost", "running", `{"has_cost":true,"cost_usd":100}`, true) // ineligible even with cost
	addJob(repo.ID, "feature", "cost", "failed", "", false)                                 // no invocation
	liveSyn := addJob(repo.ID, "feature", "live", "done", `{"has_cost":true,"cost_usd":4}`, true)
	summaryPanel(t, db, "acme/api", 4, "live", liveSyn)
	otherSyn := addJob(other.ID, "feature", "other", "done", `{"has_cost":true,"cost_usd":50}`, true)
	summaryPanel(t, db, "acme/other", 1, "other", otherSyn)
	branchSyn := addJob(repo.ID, "feature-extra", "branch", "done", `{"has_cost":true,"cost_usd":80}`, true)
	summaryPanel(t, db, "acme/api", 5, "branch", branchSyn)
	summaryPanel(t, db, "acme/orphan", 1, "orphan", int64(999999))
	s, err := db.GetSummary(SummaryOptions{Since: ciSummarySince, RepoPath: repo.RootPath, Branch: "feature"})
	require.NoError(t, err)
	require.NotNil(t, s.CI)
	a := assert.New(t)
	a.Equal(2, s.CI.TotalPanels)
	a.Equal(0, s.Overview.Total) // job timestamps do not scope CI costs
	a.Equal(CostAggregate{TotalUSD: 9, JobsTotal: 5, JobsWithCost: 4}, s.CI.Cost)
	a.Equal(CostAggregate{TotalUSD: 5, JobsTotal: 4, JobsWithCost: 3}, s.CI.RetiredUnpostedCost)
	require.Len(t, s.CI.PRs, 2)
	a.Equal(CostAggregate{TotalUSD: 5, JobsTotal: 4, JobsWithCost: 3}, s.CI.PRs[0].Cost)
	a.Equal(CostAggregate{TotalUSD: 4, JobsTotal: 1, JobsWithCost: 1, Complete: true}, s.CI.PRs[1].Cost)
	s, err = db.GetSummary(SummaryOptions{Since: ciSummarySince, Branch: "feature"})
	require.NoError(t, err)
	a.Equal(3, s.CI.TotalPanels)
	s, err = db.GetSummary(SummaryOptions{Since: ciSummarySince, RepoPath: repo.RootPath})
	require.NoError(t, err)
	a.Equal(3, s.CI.TotalPanels)
	s, err = db.GetSummary(SummaryOptions{Since: ciSummarySince})
	require.NoError(t, err)
	a.Equal(5, s.CI.TotalPanels)
}

func TestGetSummary_CITurnaroundUnavailable(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	for i, stamp := range []string{"", "invalid", "2026-01-04"} {
		id := summaryPanel(t, db, "acme/api", i+1, fmt.Sprint(i), nil)
		_, err := db.ExecContext(t.Context(), `UPDATE ci_pr_panels SET posted_at='2026-01-03',first_attempt_at=NULLIF(?, '') WHERE id=?`, stamp, id)
		require.NoError(t, err)
	}
	s, err := db.GetSummary(SummaryOptions{Since: ciSummarySince})
	require.NoError(t, err)
	assert.Equal(t, 0, s.CI.TurnaroundSamples)
	assert.Equal(t, map[string]int{"unknown": 3}, s.CI.Outcomes)
}

func FuzzAggregateCIPanels(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 1, 2, 3})
	f.Add([]byte{255, 1, 5, 2, 7, 5})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Each byte makes a tiny row; bound fixture work, not production data.
		if len(data) > 128 {
			data = data[:128]
		}
		rows := make([]ciSummaryPanel, 0, len(data))
		for _, b := range data {
			rows = append(rows, ciSummaryPanel{
				githubRepo: fmt.Sprint(b % 2), prNumber: int(b % 7), posted: b&2 != 0, retired: b&1 != 0,
				turnaround: sql.NullFloat64{Valid: b&4 != 0, Float64: float64(int8(b))},
				cost:       CostAggregate{TotalUSD: float64(b) / 100, JobsTotal: 2, JobsWithCost: 1},
			})
		}
		got := aggregateCIPanels(rows)
		slices.Reverse(rows)
		assert.Equal(t, got, aggregateCIPanels(rows))
		if len(rows) == 0 {
			assert.Nil(t, got)
			return
		}
		assert.Equal(t, len(rows), got.TotalPanels)
		assert.Equal(t, got.TotalPanels, got.ActivePanels+got.PostedPanels+got.RetiredUnpostedPanels)
		assert.LessOrEqual(t, got.RunsPerPRP50, got.RunsPerPRP90)
		assert.GreaterOrEqual(t, got.RunsPerPRP50, float64(1))
		assert.LessOrEqual(t, got.RunsPerPRP90, float64(got.RunsPerPRMax))
		assert.GreaterOrEqual(t, got.TurnaroundP50Secs, float64(0))
		assert.LessOrEqual(t, got.TurnaroundP50Secs, got.TurnaroundP90Secs)
		assert.LessOrEqual(t, got.TurnaroundP90Secs, float64(127))
		total := 0
		for _, pr := range got.PRs {
			total += pr.Runs
		}
		assert.Equal(t, len(rows), total)
	})
}

func TestAggregateCIPanels_AllHotPRsAndTies(t *testing.T) {
	t.Parallel()
	var rows []ciSummaryPanel
	for pr := 13; pr >= 1; pr-- {
		for range 6 {
			rows = append(rows, ciSummaryPanel{githubRepo: "acme/api", prNumber: pr})
		}
	}
	for range 5 {
		rows = append(rows, ciSummaryPanel{githubRepo: "acme/web", prNumber: 1})
	}
	rows = append(rows, ciSummaryPanel{githubRepo: "acme/api", prNumber: 13})
	s := aggregateCIPanels(rows)
	require.Len(t, s.HotPRs, 13)
	assert.Equal(t, 13, s.HotPRs[0].PRNumber)
	assert.Equal(t, 7, s.HotPRs[0].Runs)
	assert.Equal(t, 1, s.HotPRs[1].PRNumber)
	assert.Equal(t, 12, s.HotPRs[12].PRNumber)
	assert.Len(t, s.PRs, 14)
}

func TestGetSummary_CIPostedRetirementAndOldCosts(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, "/tmp/ci-cost-history")
	for _, key := range []string{"finalized", "outside"} {
		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: key, Agent: "test"})
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET panel_run_uuid=?,status='done',agent_invoked=1,token_usage='{"has_cost":true,"cost_usd":2}',started_at='2026-01-04',finished_at='2026-01-04' WHERE id=?`, testUUID(key), job.ID)
		require.NoError(t, err)
		id := summaryPanel(t, db, "acme/api", 1, key, job.ID)
		if key == "outside" {
			_, err = db.ExecContext(t.Context(), `UPDATE ci_pr_panels SET created_at='2026-01-01' WHERE id=?`, id)
		} else {
			_, err = db.ExecContext(t.Context(), `UPDATE ci_pr_panels SET posted_at='2026-01-04',retired_at='2026-01-05',outcome='abandoned' WHERE id=?`, id)
		}
		require.NoError(t, err)
	}
	s, err := db.GetSummary(SummaryOptions{Since: ciSummarySince})
	require.NoError(t, err)
	a := assert.New(t)
	a.Equal(1, s.CI.TotalPanels)
	a.Equal(1, s.CI.PostedPanels)
	a.Equal(0, s.CI.ClosedBeforePost)
	a.Equal(map[string]int{"unknown": 1}, s.CI.RetiredByReason)
	a.Equal(map[string]int{"abandoned": 1}, s.CI.Outcomes)
	a.Equal(CostAggregate{TotalUSD: 2, JobsTotal: 1, JobsWithCost: 1, Complete: true}, s.CI.Cost)
	a.Equal(CostAggregate{}, s.CI.RetiredUnpostedCost)
}

func TestAggregateCIPanels_ZeroTurnaround(t *testing.T) {
	t.Parallel()
	s := aggregateCIPanels([]ciSummaryPanel{{githubRepo: "acme/api", prNumber: 1, posted: true, turnaround: sql.NullFloat64{Valid: true}}})
	assert.Equal(t, 1, s.TurnaroundSamples)
	assert.InDelta(t, 0, s.TurnaroundP50Secs, 0)
	assert.InDelta(t, 0, s.TurnaroundP90Secs, 0)
}
