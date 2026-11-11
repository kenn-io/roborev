package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListJobsFailedReviewsInWindow(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, "/synthetic/repo")
	other := createRepo(t, db, "/synthetic/other")
	commit := createCommit(t, db, repo.ID, "abc123")
	since := time.Date(2026, time.January, 1, 12, 0, 0, 500_000_000, time.UTC)
	asOf := since.Add(time.Hour)
	cases := []struct {
		name, jobType string
		ref           string
		commitID      any
		verdict       any
		panelRole     string
		branch        string
		finished      string
		repoID        int64
		want          bool
	}{
		{name: "review", jobType: JobTypeReview, want: true},
		{name: "range", jobType: JobTypeRange, want: true},
		{name: "dirty", jobType: JobTypeDirty, want: true},
		{name: "synthesis", jobType: JobTypeSynthesis, panelRole: PanelRoleSynthesis, want: true},
		{name: "legacy commit", commitID: commit.ID, want: true},
		{name: "legacy dirty", ref: "dirty", want: true},
		{name: "legacy range", ref: "base..head", want: true},
		{name: "legacy task", ref: "task"},
		{name: "goal", jobType: JobTypeGoalReview},
		{name: "compact", jobType: JobTypeCompact},
		{name: "task", jobType: JobTypeTask},
		{name: "insights", jobType: JobTypeInsights},
		{name: "null verdict", jobType: JobTypeReview},
		{name: "passing", jobType: JobTypeReview, verdict: 1},
		{name: "panel member", jobType: JobTypeReview, panelRole: PanelRoleMember},
		{name: "other branch", jobType: JobTypeReview, branch: "feature"},
		{name: "other repo", jobType: JobTypeReview, repoID: other.ID},
		{name: "before window", jobType: JobTypeReview, finished: since.Add(-time.Millisecond).Format(time.RFC3339Nano)},
		{name: "at window", jobType: JobTypeReview, finished: since.Format(time.RFC3339Nano), want: true},
		{name: "after start", jobType: JobTypeReview, finished: since.Add(time.Millisecond).Format(time.RFC3339Nano), want: true},
		{name: "offset at window", jobType: JobTypeReview, finished: "2026-01-01T07:00:00.500-05:00", want: true},
		{name: "at end", jobType: JobTypeReview, finished: asOf.Format(time.RFC3339Nano), want: true},
		{name: "after end", jobType: JobTypeReview, finished: asOf.Add(time.Millisecond).Format(time.RFC3339Nano)},
	}
	var wantIDs []int64
	for i, tc := range cases {
		if tc.repoID == 0 {
			tc.repoID = repo.ID
		}
		if tc.branch == "" {
			tc.branch = "main"
		}
		if tc.ref == "" {
			tc.ref = "abc123"
		}
		if tc.finished == "" {
			tc.finished = asOf.Format(time.RFC3339Nano)
		}
		if tc.verdict == nil && tc.name != "null verdict" {
			tc.verdict = 0
		}
		_, err := db.ExecContext(t.Context(), `INSERT INTO review_jobs
			(id, repo_id, commit_id, git_ref, branch, agent, status, job_type, panel_role, finished_at, enqueued_at)
			VALUES (?, ?, ?, ?, ?, 'test', 'done', ?, ?, ?, ?)`,
			i+1, tc.repoID, tc.commitID, tc.ref, tc.branch, tc.jobType, tc.panelRole, tc.finished,
			since.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano))
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `INSERT INTO reviews (job_id, agent, output, verdict_bool)
			VALUES (?, 'test', 'Synthetic review', ?)`, i+1, tc.verdict)
		require.NoError(t, err)
		if tc.want {
			wantIDs = append([]int64{int64(i + 1)}, wantIDs...)
		}
	}
	opts := []ListJobsOption{WithoutPrompt(), WithBranch("main"), WithExcludePanelRole(PanelRoleMember), WithFailedReviewsInWindow(since, asOf)}
	jobs, err := db.ListJobs(string(JobStatusDone), repo.RootPath, 0, 0, opts...)
	require.NoError(t, err)
	var gotIDs []int64
	for _, job := range jobs {
		gotIDs = append(gotIDs, job.ID)
	}
	assert.Equal(t, wantIDs, gotIDs)
	limited, err := db.ListJobs(string(JobStatusDone), repo.RootPath, 2, 0, opts...)
	require.NoError(t, err)
	require.Len(t, limited, 2)
	for i, job := range limited {
		assert.Equal(t, wantIDs[i], job.ID)
	}
}
