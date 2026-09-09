package storage

import (
	"database/sql"
	"testing"
	"uuid"

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
		{name: "empty structured output falls back to prose", structured: new(""), prose: "- High — old finding", want: &FindingCounts{High: 1, Approximate: true}},
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
		{name: "range done", job: ReviewJob{JobType: JobTypeRange, Status: JobStatusDone}, want: true},
		{name: "dirty done", job: ReviewJob{JobType: JobTypeDirty, Status: JobStatusDone}, want: true},
		{name: "compact applied", job: ReviewJob{JobType: JobTypeCompact, Status: JobStatusApplied}, want: true},
		{name: "synthesis rebased", job: ReviewJob{JobType: JobTypeSynthesis, Status: JobStatusRebased}, want: true},
		{name: "legacy review", job: ReviewJob{CommitID: &commitID, Status: JobStatusDone}, want: true},
		{name: "task", job: ReviewJob{JobType: JobTypeTask, Status: JobStatusDone}, want: false},
		{name: "insights", job: ReviewJob{JobType: JobTypeInsights, Status: JobStatusDone}, want: false},
		{name: "fix", job: ReviewJob{JobType: JobTypeFix, Status: JobStatusDone}, want: false},
		{name: "classify", job: ReviewJob{JobType: JobTypeClassify, Status: JobStatusDone}, want: false},
		{name: "queued review", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusQueued}, want: false},
		{name: "running review", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusRunning}, want: false},
		{name: "failed review", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusFailed}, want: false},
		{name: "canceled review", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusCanceled}, want: false},
		{name: "applied review", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusApplied}, want: true},
		{name: "rebased review", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusRebased}, want: true},
		{name: "skipped review", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusSkipped}, want: false},
		{name: "review error", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusDone, Error: "error"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.job.HasFindingCountsOutput())
		})
	}
}

func TestJobFindingCountsMetadataVariants(t *testing.T) {
	sourceMachineID := uuid.UUID{2}
	resumeSourceJobUUID := uuid.UUID{3}
	tests := []struct {
		name string
		job  ReviewJob
	}{
		{name: "user source", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusDone}},
		{name: "auto design source", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusDone, Source: JobSourceAutoDesign}},
		{name: "ci source", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusDone, Source: JobSourceCI, CIBaseBranch: "main"}},
		{name: "post commit source", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusDone, Source: JobSourcePostCommit}},
		{name: "synced remote row", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusDone, SourceMachineID: &sourceMachineID}},
		{name: "rerun unseen", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusDone}},
		{name: "rerun seen", job: ReviewJob{JobType: JobTypeReview, Status: JobStatusDone, ResumeSourceJobUUID: &resumeSourceJobUUID}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, tt.job.HasFindingCountsOutput())
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
	_, err = db.Exec("UPDATE review_jobs SET diff_content = ? WHERE id = ?", "large legacy diff payload", job.ID)
	require.NoError(t, err)

	defaultJobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	require.Len(t, defaultJobs, 1)
	assert.Nil(t, defaultJobs[0].FindingCounts)
	assert.Nil(t, defaultJobs[0].DiffContent)

	jobs, err := db.ListJobs("", "", 0, 0, WithFindingCounts())
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, &FindingCounts{High: 1, Medium: 1}, jobs[0].FindingCounts)
	require.NotNil(t, jobs[0].DiffContent)
	assert.Equal(t, "large legacy diff payload", *jobs[0].DiffContent)

	defaultReview, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Nil(t, defaultReview.Job.DiffContent)
	countedReview, err := db.GetReviewByJobIDWithFindingCounts(job.ID)
	require.NoError(t, err)
	require.NotNil(t, countedReview.Job.DiffContent)
	assert.Equal(t, "large legacy diff payload", *countedReview.Job.DiffContent)

	_, err = db.Exec("UPDATE reviews SET structured_output = NULL, output = ? WHERE job_id = ?", "- High — old finding", job.ID)
	require.NoError(t, err)
	jobs, err = db.ListJobs("", "", 0, 0, WithFindingCounts())
	require.NoError(t, err)
	assert.Equal(t, &FindingCounts{High: 1, Approximate: true}, jobs[0].FindingCounts)

	_, err = db.Exec("UPDATE reviews SET structured_output = ?, output = ? WHERE job_id = ?", "", "- High — empty structured output", job.ID)
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

	_, err = db.Exec("UPDATE review_jobs SET job_type = '', commit_id = NULL, git_ref = '', diff_content = ? WHERE id = ?", "diff --git a/file.go b/file.go", job.ID)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE reviews SET structured_output = NULL, output = ?, verdict_bool = NULL WHERE job_id = ?", "- Medium — legacy diff-only finding", job.ID)
	require.NoError(t, err)
	jobs, err = db.ListJobs("", "", 0, 0, WithFindingCounts())
	require.NoError(t, err)
	assert.Equal(t, &FindingCounts{Medium: 1, Approximate: true}, jobs[0].FindingCounts)
	review, err := db.GetReviewByJobIDWithFindingCounts(job.ID)
	require.NoError(t, err)
	assert.Equal(t, &FindingCounts{Medium: 1, Approximate: true}, review.Job.FindingCounts)
}
