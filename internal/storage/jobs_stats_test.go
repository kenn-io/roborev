package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJobStatsQueryReadsMetadataIndexes(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	t.Run("fresh", func(t *testing.T) {
		// Counting the queue must not read past each historical job/review's
		// prompt to reach its panel role, source, or closed state.
		for _, tc := range []struct {
			name   string
			status string
			opts   []ListJobsOption
		}{
			{name: "all", opts: []ListJobsOption{WithExcludePanelRole(PanelRoleMember)}},
			{name: "browser", opts: []ListJobsOption{WithExcludePanelRole(PanelRoleMember), WithHideClassifyJobs()}},
			{name: "open", opts: []ListJobsOption{WithExcludePanelRole(PanelRoleMember), WithHideClassifyJobs(), WithClosed(false)}},
			{name: "status", status: "done", opts: []ListJobsOption{WithExcludePanelRole(PanelRoleMember), WithHideClassifyJobs()}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				query, args := jobStatsQuery(tc.status, "", tc.opts...)
				plan := queryPlan(t, db, query, args...)
				assert.Contains(t, plan, "j USING COVERING INDEX idx_review_jobs_queue_stats", "queue counts must skip job payloads")
				assert.NotContains(t, plan, "rv")
				assert.NotContains(t, plan, "r USING")
				if tc.status != "" {
					assert.Contains(t, plan, "SEARCH j USING COVERING INDEX idx_review_jobs_queue_stats (status=?)")
					// Closed counts must also avoid fetching review_closed from job rows.
					query, args = jobStatsQuery(tc.status, "", append(tc.opts, WithClosed(true))...)
					plan = queryPlan(t, db, query, args...)
					assert.Contains(t, plan, "j USING COVERING INDEX idx_review_jobs_queue_stats", "closed counts must skip job payloads")
					assert.NotContains(t, plan, "rv")
					assert.NotContains(t, plan, "r USING")
				}
			})
		}
	})
}

func TestJobStatsTotalAndRepos(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	for _, path := range []string{"/repos/a", "/repos/b"} {
		repo, err := db.GetOrCreateRepo(path)
		require.NoError(t, err)
		for _, status := range []JobStatus{JobStatusDone, JobStatusApplied, JobStatusRebased} {
			job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: string(status), Agent: "test"})
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), "UPDATE review_jobs SET status = ?, review_closed = ? WHERE id = ?", status, path == "/repos/b", job.ID)
			require.NoError(t, err)
		}
	}
	for _, tc := range []struct {
		repo, status string
		opts         []ListJobsOption
		total, repos int
	}{
		{total: 6, repos: 2},
		{repo: "/repos/a", total: 3, repos: 1},
		{status: "applied", total: 2, repos: 2},
		{opts: []ListJobsOption{WithClosed(false)}, total: 3, repos: 1},
	} {
		stats, err := db.CountJobStats(tc.status, tc.repo, tc.opts...)
		require.NoError(t, err)
		assert.Equal(t, tc.total, stats.Total)
		assert.Equal(t, tc.repos, stats.RepoCount)
	}
}
