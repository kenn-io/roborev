package storage

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewFindingCounts(t *testing.T) {
	structured := `{"schema_version":2,"summary":"review","verdict":"fail","findings":[{"severity":"critical","problem":"p","fix":"f","location":null},{"severity":"high","problem":"p","fix":"f","location":null},{"severity":"medium","problem":"p","fix":"f","location":null},{"severity":"low","problem":"p","fix":"f","location":null}]}`
	structuredV1 := `{"schema_version":1,"summary":"review","findings":[{"severity":"low","problem":"p","fix":"f","location":null}]}`

	tests := []struct {
		name       string
		structured *string
		prose      string
		want       *FindingCounts
	}{
		{name: "structured v2", structured: &structured, want: &FindingCounts{Critical: 1, High: 1, Medium: 1, Low: 1}},
		{name: "structured v1", structured: &structuredV1, want: &FindingCounts{Low: 1}},
		{name: "valid empty structured output", structured: new(`{"schema_version":2,"summary":"clean","verdict":"pass","findings":[]}`), want: &FindingCounts{}},
		{name: "unable to review", structured: new(`{"schema_version":2,"summary":"unavailable","verdict":"unable_to_review","findings":[]}`)},
		{name: "malformed structured output", structured: new(`{"schema_version":2`)},
		{name: "non-object structured output", structured: new(`[]`)},
		{name: "trailing structured output", structured: new(structured + ` {}`)},
		{
			name:  "prose labels exclude rubric and high-level",
			prose: "Severity rubric:\n- High — rubric\n- Medium — rubric\n## Findings\n- High — bug\n- Medium: issue\nLow — nit\nHigh-level overview",
			want:  &FindingCounts{High: 1, Medium: 1, Low: 1, Approximate: true},
		},
		{name: "unlabeled prose", prose: "No issues found."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ReviewFindingCounts(tt.structured, tt.prose))
		})
	}
}

func TestJobFindingCountsEligibility(t *testing.T) {
	commitID := int64(1)
	tests := []struct {
		name string
		job  ReviewJob
		want bool
	}{
		{name: "review done", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusDone}, want: true},
		{name: "compact applied", job: ReviewJob{JobType: JobTypeCompact, Status: JobStatusApplied}, want: true},
		{name: "synthesis rebased", job: ReviewJob{JobType: JobTypeSynthesis, Status: JobStatusRebased}, want: true},
		{name: "legacy review", job: ReviewJob{CommitID: &commitID, Status: JobStatusDone}, want: true},
		{name: "task", job: ReviewJob{JobType: JobTypeTask, Status: JobStatusDone}, want: false},
		{name: "insights", job: ReviewJob{JobType: JobTypeInsights, Status: JobStatusDone}, want: false},
		{name: "fix", job: ReviewJob{JobType: JobTypeFix, Status: JobStatusDone}, want: false},
		{name: "classify", job: ReviewJob{JobType: JobTypeClassify, Status: JobStatusDone}, want: false},
		{name: "queued review", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusQueued}, want: false},
		{name: "failed review", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusFailed}, want: false},
		{name: "review error", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusDone, Error: "error"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.job.HasFindingCountsOutput())
		})
	}
}

func TestListJobsFindingCounts(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo := createRepo(t, db, "/tmp/finding-counts")
	commit := createCommit(t, db, repo.ID, "finding-counts-sha")
	job := enqueueJob(t, db, repo.ID, commit.ID, commit.SHA)
	require.NotNil(t, claimJob(t, db, "finding-counts-worker"))
	require.NoError(t, db.CompleteJob(job.ID, "codex", "prompt", "No issues found."))
	structured := `{"schema_version":2,"summary":"review","verdict":"fail","findings":[{"severity":"high","problem":"p","fix":"f","location":null},{"severity":"medium","problem":"p","fix":"f","location":null}]}`
	_, err := db.Exec("UPDATE reviews SET structured_output = ? WHERE job_id = ?", structured, job.ID)
	require.NoError(t, err)

	defaultJobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	require.Len(t, defaultJobs, 1)
	assert.Nil(t, defaultJobs[0].FindingCounts)

	jobs, err := db.ListJobs("", "", 0, 0, WithFindingCounts())
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, &FindingCounts{High: 1, Medium: 1}, jobs[0].FindingCounts)

	_, err = db.Exec("UPDATE reviews SET structured_output = NULL, output = ? WHERE job_id = ?", "- High — old finding", job.ID)
	require.NoError(t, err)
	jobs, err = db.ListJobs("", "", 0, 0, WithFindingCounts())
	require.NoError(t, err)
	assert.Equal(t, &FindingCounts{High: 1, Approximate: true}, jobs[0].FindingCounts)

	_, err = db.Exec("UPDATE reviews SET structured_output = ? WHERE job_id = ?", "{", job.ID)
	require.NoError(t, err)
	jobs, err = db.ListJobs("", "", 0, 0, WithFindingCounts())
	require.NoError(t, err)
	assert.Nil(t, jobs[0].FindingCounts)

	var structuredOutput sql.NullString
	require.NoError(t, db.QueryRow("SELECT structured_output FROM reviews WHERE job_id = ?", job.ID).Scan(&structuredOutput))
	assert.True(t, structuredOutput.Valid)
}
