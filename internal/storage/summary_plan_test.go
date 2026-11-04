package storage

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Check the SQL issued by the aggregates, including their status and agent
// predicates, so a competing index cannot silently restore historical scans.
type sincePlanQuerier struct {
	querier
	t     *testing.T
	index string
}

func (q sincePlanQuerier) check(query string, args []any) {
	q.t.Helper()
	if q.index == "" {
		assert.Contains(q.t, query, "datetime(j.enqueued_at) >= datetime(?)")
		assert.NotContains(q.t, query, "INDEXED BY")
		return
	}
	plan := queryPlan(q.t, q.querier, query, args...)
	assert.Contains(q.t, plan, "SEARCH j USING INDEX "+q.index, query)
	assert.Contains(q.t, plan, "<expr>>?", query)
	assert.NotContains(q.t, plan, "SCAN j", query)
}

func (q sincePlanQuerier) Query(query string, args ...any) (*sql.Rows, error) {
	q.check(query, args)
	return q.querier.Query(query, args...)
}

func (q sincePlanQuerier) QueryRow(query string, args ...any) *sql.Row {
	q.check(query, args)
	return q.querier.QueryRow(query, args...)
}

func TestSummaryAndCostSinceUseEnqueuedIndex(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, scope := range []struct {
		name   string
		repo   string
		branch string
	}{
		{name: "all"},
		{name: "branch", branch: "selective"},
		{name: "repo", repo: "/tmp/summary-plan"},
		{name: "repo-branch", repo: "/tmp/summary-plan", branch: "main"},
	} {
		t.Run(scope.name, func(t *testing.T) {
			db := openTestDB(t)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			index := "idx_review_jobs_enqueued_position"
			if scope.repo != "" {
				index = "idx_review_jobs_repo_enqueued"
			}
			if scope.repo == "" && scope.branch != "" {
				index = ""
			}
			q := sincePlanQuerier{querier: db, t: t, index: index}
			from, where, args := summaryWhere(SummaryOptions{Since: since, RepoPath: scope.repo, Branch: scope.branch})
			_, err := summaryOverview(q, from, where, args)
			require.NoError(t, err)
			_, err = summaryVerdicts(q, from, where, args)
			require.NoError(t, err)
			_, err = summaryAgents(q, from, where, args)
			require.NoError(t, err)
			// Exercise the per-agent query even when the aggregate returns no agents.
			_, err = agentMedianDuration(q, from, where, args, "test")
			require.NoError(t, err)
			_, err = summaryDurations(q, from, where, args)
			require.NoError(t, err)
			_, err = summaryJobTypes(q, from, where, args)
			require.NoError(t, err)
			_, err = summaryFailures(q, from, where, args)
			require.NoError(t, err)
			_, err = summaryRepos(q, from, where, args)
			require.NoError(t, err)
			opts := CostOptions{Since: since, Branch: scope.branch}
			if scope.repo != "" {
				opts.RepoPaths = []string{scope.repo}
			}
			_, err = costAggregate(q, opts)
			require.NoError(t, err)
			if scope.repo == "" {
				opts.BranchEmpty = true
				q.index = ""
				_, err = costAggregate(q, opts)
				require.NoError(t, err)
			}
			if scope.repo != "" || scope.branch == "" {
				opts.RepoPaths = append(opts.RepoPaths, "/tmp/another-repo")
				q.index = "idx_review_jobs_repo_enqueued"
				opts.BranchEmpty = true
				_, err = costAggregate(q, opts)
				require.NoError(t, err)
			}
		})
	}
}
