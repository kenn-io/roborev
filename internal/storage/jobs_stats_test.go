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

	for _, phase := range []string{"fresh", "upgrade", "repeat"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "upgrade" {
				_, err := db.ExecContext(t.Context(), "DROP INDEX idx_review_jobs_queue_stats")
				require.NoError(t, err)
			}
			if phase != "fresh" {
				require.NoError(t, db.migrate())
			}
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
				{name: "status closed", status: "done", opts: []ListJobsOption{WithExcludePanelRole(PanelRoleMember), WithHideClassifyJobs(), WithClosed(true)}},
				{name: "status", status: "done", opts: []ListJobsOption{WithExcludePanelRole(PanelRoleMember), WithHideClassifyJobs()}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					query, args := jobStatsQuery(tc.status, "", tc.opts...)
					plan := queryPlan(t, db, query, args...)
					assert.Contains(t, plan, "j USING COVERING INDEX", "queue counts must skip job payloads")
					assert.Contains(t, plan, "rv USING COVERING INDEX", "queue counts must skip review payloads")
				})
			}
		})
	}
}
