package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListJobsOpenFailedReviews(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	var openFailedID int64
	for _, tc := range []struct {
		name    string
		closed  int
		verdict any
	}{
		{"open failed", 0, 0},
		{"open passing", 0, 1},
		{"closed failed", 1, 0},
		{"unknown verdict", 0, nil},
	} {
		job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: tc.name, Branch: "main", Agent: "test"})
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET status = 'done' WHERE id = ?`, job.ID)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `
			INSERT INTO reviews (job_id, agent, output, closed, verdict_bool, structured_output)
			VALUES (?, 'test', '', ?, ?, '{"summary":"Review complete","findings":[]}')`, job.ID, tc.closed, tc.verdict)
		require.NoError(t, err)
		if tc.name == "open failed" {
			openFailedID = job.ID
		}
	}
	jobs, err := db.ListJobs("done", repo.RootPath, 0, 0, WithOpenFailedReviews())
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, openFailedID, jobs[0].ID)
	assert.Equal(t, new("F"), jobs[0].Verdict)
}

func TestListJobsOpenFailedReviewsQueryPlan(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	query, args, _, err := listJobsQuery("done", t.TempDir(), 0, 0, collectListJobsOptions(
		WithoutPrompt(), WithClosed(false), WithOpenFailedReviews(),
		WithBranchOrEmpty("main"), WithExcludePanelRole(PanelRoleMember),
	))
	require.NoError(t, err)
	plan := queryPlan(t, db, query, args...)
	assert.Regexp(t, `USING (COVERING )?INDEX idx_reviews_open_failed`, plan)
	assert.Contains(t, plan, "j USING INTEGER PRIMARY KEY")
}
