package storage

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetAnalyticsClassifiesReviewsAttemptsAndProjects(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	base := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	repoA := createRepo(t, db, filepath.Join(t.TempDir(), "project-a"))
	repoB := createRepo(t, db, filepath.Join(t.TempDir(), "project-a"))
	repoC := createRepo(t, db, filepath.Join(t.TempDir(), "project-c"))
	for _, repo := range []*Repo{repoA, repoB} {
		_, err := db.Exec(`UPDATE repos SET name = 'shared-project' WHERE id = ?`, repo.ID)
		require.NoError(t, err)
	}

	seedAnalyticsJob(t, db, repoA, analyticsJobSeed{
		name: "pass", jobType: JobTypeReview, status: JobStatusDone,
		source: JobSourcePostCommit, agent: "agent-a", model: "model-a",
		enqueuedAt: base.Add(time.Hour), startedAt: base.Add(2 * time.Hour),
		finishedAt: base.Add(3 * time.Hour), verdict: new(1),
		costJSON: `{"cost_usd":1.25,"has_cost":true}`,
	})
	seedAnalyticsJob(t, db, repoB, analyticsJobSeed{
		name: "fail-open", jobType: JobTypeDirty, status: JobStatusDone,
		source: JobSourcePostCommit, agent: "agent-b", model: "model-b",
		enqueuedAt: base.Add(4 * time.Hour), startedAt: base.Add(5 * time.Hour),
		finishedAt: base.Add(7 * time.Hour), verdict: new(0), invoked: true,
	})
	closed := seedAnalyticsJob(t, db, repoA, analyticsJobSeed{
		name: "fail-closed", jobType: JobTypeCompact, status: JobStatusDone,
		source: JobSourceCI, agent: "agent-a", model: "model-a",
		enqueuedAt: base.Add(8 * time.Hour), startedAt: base.Add(9 * time.Hour),
		finishedAt: base.Add(13 * time.Hour), verdict: new(0), closed: true,
		costJSON: `{"cost_usd":0,"has_cost":true}`,
	})
	seedAnalyticsJob(t, db, repoC, analyticsJobSeed{
		name: "failed", jobType: JobTypeReview, status: JobStatusFailed,
		source: JobSourceCI, agent: "agent-c", model: "model-c",
		enqueuedAt: base.Add(14 * time.Hour), startedAt: base.Add(15 * time.Hour),
		finishedAt: base.Add(16 * time.Hour), invoked: true,
	})
	seedAnalyticsJob(t, db, repoC, analyticsJobSeed{
		name: "canceled", jobType: JobTypeRange, status: JobStatusCanceled,
		source: JobSourceAutoDesign, agent: "agent-c", model: "model-c",
		enqueuedAt: base.Add(17 * time.Hour), startedAt: base.Add(18 * time.Hour),
		finishedAt: base.Add(19 * time.Hour), invoked: true,
	})
	seedAnalyticsJob(t, db, repoC, analyticsJobSeed{
		name: "skipped", jobType: JobTypeReview, status: JobStatusSkipped,
		source: JobSourceAutoDesign, agent: "agent-c", model: "model-c",
		enqueuedAt: base.Add(20 * time.Hour), finishedAt: base.Add(21 * time.Hour),
	})
	seedAnalyticsJob(t, db, repoC, analyticsJobSeed{
		name: "task", jobType: JobTypeTask, status: JobStatusDone,
		enqueuedAt: base.Add(22 * time.Hour), startedAt: base.Add(22 * time.Hour),
		finishedAt: base.Add(23 * time.Hour), verdict: new(0), invoked: true,
	})
	seedAnalyticsJob(t, db, repoA, analyticsJobSeed{
		name: "panel-member", jobType: JobTypeReview, panelRole: PanelRoleMember,
		status: JobStatusDone, source: JobSourcePostCommit, agent: "agent-a", model: "model-a",
		enqueuedAt: base.Add(10 * time.Hour), startedAt: base.Add(10 * time.Hour),
		finishedAt: base.Add(11 * time.Hour), costJSON: `{"cost_usd":0.75,"has_cost":true}`,
	})
	seedAnalyticsJob(t, db, repoA, analyticsJobSeed{
		name: "passthrough", jobType: JobTypeSynthesis, panelRole: PanelRoleSynthesis,
		status: JobStatusDone, source: JobSourcePostCommit,
		enqueuedAt: base.Add(10 * time.Hour), startedAt: base.Add(10 * time.Hour),
		finishedAt: base.Add(12 * time.Hour), verdict: new(1),
	})

	got, err := db.GetAnalytics(AnalyticsOptions{
		Since: base, Until: base.Add(24 * time.Hour), Bucket: AnalyticsBucketDay,
	})
	require.NoError(t, err)

	assert.Equal(7, got.Summary.Reviews.Total)
	assert.Equal(4, got.Summary.Reviews.Done)
	assert.Equal(1, got.Summary.Reviews.Failed)
	assert.Equal(1, got.Summary.Reviews.Canceled)
	assert.Equal(1, got.Summary.Reviews.Skipped)
	assert.InDelta(0.2, got.Summary.Reviews.FailureRate, 0.0001)
	assert.Equal(2, got.Summary.Verdicts.Passed)
	assert.Equal(1, got.Summary.Verdicts.FailOpen)
	assert.Equal(1, got.Summary.Verdicts.FailClosed)
	assert.InDelta(2.5*3600, got.Summary.ReviewLatency.P50Secs, 0.001)
	assert.InDelta(4.4*3600, got.Summary.ReviewLatency.P90Secs, 0.001)
	assert.InDelta(4.94*3600, got.Summary.ReviewLatency.P99Secs, 0.001)
	assert.Equal(7, got.Summary.Attempts.Eligible)
	assert.Equal(3, got.Summary.Cost.PricedAttempts)
	assert.InDelta(2.00, got.Summary.Cost.TotalUSD, 0.0001)
	assert.InDelta(3.0/7.0, got.Summary.Cost.Coverage, 0.0001)
	require.Len(t, got.Projects, 2)
	assert.Equal("shared-project", got.Projects[0].Project)
	assert.Equal(4, got.Projects[0].Reviews.Total)
	assert.InDelta(2.00, got.Projects[0].Cost.TotalUSD, 0.0001)
	assert.Equal(7, got.TimeSeries[0].Reviews.Total)
	assert.NotContains(got.Options.Agents, "")
	assert.NotContains(got.Options.Models, "")

	_, err = db.Exec(`UPDATE reviews SET closed = 0 WHERE job_id = ?`, closed.ID)
	require.NoError(t, err)
	updated, err := db.GetAnalytics(AnalyticsOptions{
		Since: base, Until: base.Add(24 * time.Hour), Bucket: AnalyticsBucketDay,
	})
	require.NoError(t, err)
	assert.Equal(2, updated.Summary.Verdicts.FailOpen)
	assert.Equal(0, updated.Summary.Verdicts.FailClosed)
}

func TestGetAnalyticsUsesVerdictsForFailureRateAcrossSuccessfulTerminalStates(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	base := time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC)
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "project-a"))
	seeds := []analyticsJobSeed{
		{name: "pass", jobType: JobTypeReview, status: JobStatusDone, verdict: new(1)},
		{name: "fail-open", jobType: JobTypeReview, status: JobStatusApplied, verdict: new(0)},
		{name: "fail-closed", jobType: JobTypeReview, status: JobStatusRebased, verdict: new(0), closed: true},
		{name: "unrated", jobType: JobTypeReview, status: JobStatusDone},
		{name: "run-error", jobType: JobTypeReview, status: JobStatusFailed},
	}
	for index, seed := range seeds {
		seed.enqueuedAt = base.Add(time.Duration(index) * time.Hour)
		seed.startedAt = seed.enqueuedAt.Add(time.Minute)
		seed.finishedAt = seed.enqueuedAt.Add(2 * time.Minute)
		seedAnalyticsJob(t, db, repo, seed)
	}

	got, err := db.GetAnalytics(AnalyticsOptions{
		Since: base, Until: base.Add(24 * time.Hour), Bucket: AnalyticsBucketDay,
	})
	require.NoError(t, err)

	assert.Equal(5, got.Summary.Reviews.Total)
	assert.Equal(4, got.Summary.Reviews.Done)
	assert.Equal(1, got.Summary.Reviews.Failed)
	assert.InDelta(0.2, got.Summary.Reviews.FailureRate, 0.0001)
	assert.Equal(1, got.Summary.Reviews.RunErrors)
	assert.InDelta(0.2, got.Summary.Reviews.RunErrorRate, 0.0001)
	assert.Equal(1, got.Summary.Verdicts.Passed)
	assert.Equal(1, got.Summary.Verdicts.FailOpen)
	assert.Equal(1, got.Summary.Verdicts.FailClosed)
	assert.Equal(3, got.Summary.Verdicts.Rated)
	assert.InDelta(2.0/3.0, got.Summary.Verdicts.FailureRate, 0.0001)
}

func TestGetAnalyticsIncludesInvokedSkippedAttempts(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	base := time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC)
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "project-a"))

	seedAnalyticsJob(t, db, repo, analyticsJobSeed{
		name: "classifier-ran", jobType: JobTypeReview, status: JobStatusSkipped,
		agent: "agent-a", model: "model-a", invoked: true,
		enqueuedAt: base, startedAt: base.Add(time.Minute), finishedAt: base.Add(2 * time.Minute),
		costJSON: `{"cost_usd":0.5,"has_cost":true}`,
	})
	seedAnalyticsJob(t, db, repo, analyticsJobSeed{
		name: "pre-agent-skip", jobType: JobTypeReview, status: JobStatusSkipped,
		enqueuedAt: base.Add(time.Hour), finishedAt: base.Add(time.Hour + time.Minute),
	})

	got, err := db.GetAnalytics(AnalyticsOptions{
		Since: base, Until: base.Add(2 * time.Hour), Bucket: AnalyticsBucketHour,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, got.Summary.Attempts.Eligible)
	assert.Equal(t, 1, got.Summary.Cost.PricedAttempts)
	assert.InDelta(t, 0.5, got.Summary.Cost.TotalUSD, 0.0001)
	require.Len(t, got.Agents, 1)
	assert.Equal(t, 1, got.Agents[0].Attempts.Eligible)
}

func TestGetAnalyticsAttributesInvokedClassifierSkip(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	jobID := seedRunningClassify(
		t, db, filepath.Join(t.TempDir(), "classifier-project"), "classifier-sha", "worker-1",
	)
	require.NoError(t, db.MarkClassifyAgentInvoked(
		jobID, "worker-1", "classifier-agent", "classifier-model", "classifier command",
	))
	require.NoError(t, db.MarkClassifyAsSkippedDesign(
		jobID, "worker-1", "design review not needed", "",
	))

	now := time.Now().UTC()
	got, err := db.GetAnalytics(AnalyticsOptions{
		Since:  now.Add(-time.Minute),
		Until:  now.Add(time.Minute),
		Bucket: AnalyticsBucketHour,
		Agents: []string{"classifier-agent"},
		Models: []string{"classifier-model"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, got.Summary.Attempts.Eligible)
	require.Len(t, got.Agents, 1)
	assert.Equal(t, "classifier-agent", got.Agents[0].Value)
	require.Len(t, got.Models, 1)
	assert.Equal(t, "classifier-model", got.Models[0].Value)
}

func TestGetAnalyticsPreservesFractionalSecondBounds(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	base := time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC)
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "project"))
	job := seedAnalyticsJob(t, db, repo, analyticsJobSeed{
		name: "fractional", jobType: JobTypeReview, status: JobStatusDone,
		enqueuedAt: base, startedAt: base, finishedAt: base, verdict: new(1),
	})
	_, err := db.Exec(
		`UPDATE review_jobs SET finished_at = ? WHERE id = ?`,
		"2026-08-03 00:00:00.500", job.ID,
	)
	require.NoError(t, err)

	got, err := db.GetAnalytics(AnalyticsOptions{
		Since:  base.Add(250 * time.Millisecond),
		Until:  base.Add(750 * time.Millisecond),
		Bucket: AnalyticsBucketHour,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, got.Summary.Reviews.Total)

	excluded, err := db.GetAnalytics(AnalyticsOptions{
		Since:  base,
		Until:  base.Add(500 * time.Millisecond),
		Bucket: AnalyticsBucketHour,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, excluded.Summary.Reviews.Total)
}

func TestGetAnalyticsEmitsContinuousTimeBuckets(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	base := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "project"))
	for day, name := range map[int]string{0: "first", 2: "third"} {
		at := base.AddDate(0, 0, day).Add(time.Hour)
		seedAnalyticsJob(t, db, repo, analyticsJobSeed{
			name: name, jobType: JobTypeReview, status: JobStatusDone,
			enqueuedAt: at.Add(-time.Minute), startedAt: at.Add(-time.Minute),
			finishedAt: at, verdict: new(1),
		})
	}

	got, err := db.GetAnalytics(AnalyticsOptions{
		Since: base, Until: base.AddDate(0, 0, 3), Bucket: AnalyticsBucketDay,
	})
	require.NoError(t, err)
	require.Len(t, got.TimeSeries, 3)
	assert.Equal(t, base, got.TimeSeries[0].Start)
	assert.Equal(t, 1, got.TimeSeries[0].Reviews.Total)
	assert.Equal(t, base.AddDate(0, 0, 1), got.TimeSeries[1].Start)
	assert.Equal(t, AnalyticsSummary{}, got.TimeSeries[1].AnalyticsSummary)
	assert.Equal(t, base.AddDate(0, 0, 2), got.TimeSeries[2].Start)
	assert.Equal(t, 1, got.TimeSeries[2].Reviews.Total)
}

func TestGetAnalyticsRejectsExcessiveTimeBuckets(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	base := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)

	_, err := db.GetAnalytics(AnalyticsOptions{
		Since:  base,
		Until:  base.Add((MaxAnalyticsTimeBuckets + 1) * time.Hour),
		Bucket: AnalyticsBucketHour,
	})
	require.ErrorIs(t, err, ErrAnalyticsRangeTooLarge)
}

func TestGetAnalyticsIgnoresIneligibleJobsWhenBuildingDimensions(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	base := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	oldRepo := createRepo(t, db, filepath.Join(t.TempDir(), "old-project"))
	recentRepo := createRepo(t, db, filepath.Join(t.TempDir(), "recent-project"))
	recent := base.Add((MaxAnalyticsTimeBuckets + 2) * time.Hour)

	seedAnalyticsJob(t, db, oldRepo, analyticsJobSeed{
		name: "irrelevant-task", jobType: JobTypeTask, status: JobStatusDone,
		source: JobSourceCI, enqueuedAt: base.Add(-time.Minute), finishedAt: base,
	})
	seedAnalyticsJob(t, db, recentRepo, analyticsJobSeed{
		name: "recent-review", jobType: JobTypeReview, status: JobStatusDone,
		source: JobSourcePostCommit, enqueuedAt: recent.Add(-time.Minute),
		startedAt: recent.Add(-time.Minute), finishedAt: recent, verdict: new(1),
	})

	got, err := db.GetAnalytics(AnalyticsOptions{
		Until: recent.Add(time.Hour), Bucket: AnalyticsBucketHour,
	})
	require.NoError(t, err)
	require.Len(t, got.Projects, 1)
	assert.Equal(t, recentRepo.Name, got.Projects[0].Project)
	require.Len(t, got.Sources, 1)
	assert.Equal(t, JobSourcePostCommit, got.Sources[0].Value)
	require.Len(t, got.TimeSeries, 1)
	assert.Equal(t, 1, got.TimeSeries[0].Reviews.Total)
}

func TestGetAnalyticsFiltersPopulationsIndependently(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	base := time.Date(2026, time.August, 2, 0, 0, 0, 0, time.UTC)
	repo := createRepo(t, db, filepath.Join(t.TempDir(), "project"))
	_, err := db.Exec(`UPDATE repos SET name = 'project' WHERE id = ?`, repo.ID)
	require.NoError(t, err)

	seedAnalyticsJob(t, db, repo, analyticsJobSeed{
		name: "first", jobType: JobTypeReview, status: JobStatusDone,
		source: JobSourceCI, agent: "agent-a", model: "model-a",
		enqueuedAt: base, startedAt: base, finishedAt: base.Add(time.Hour),
		verdict: new(1), costJSON: `{"cost_usd":1,"has_cost":true}`,
	})
	seedAnalyticsJob(t, db, repo, analyticsJobSeed{
		name: "until-exclusive", jobType: JobTypeReview, status: JobStatusDone,
		source: JobSourceCI, agent: "agent-b", model: "model-b",
		enqueuedAt: base.Add(time.Hour), startedAt: base.Add(time.Hour),
		finishedAt: base.Add(2 * time.Hour), verdict: new(0),
		costJSON: `{"cost_usd":2,"has_cost":true}`,
	})

	got, err := db.GetAnalytics(AnalyticsOptions{
		Since: base, Until: base.Add(3 * time.Hour), Projects: []string{"project"},
		Sources: []string{JobSourceCI}, Agents: []string{"agent-a"}, Models: []string{"model-a"},
		Bucket: AnalyticsBucketHour,
	})
	require.NoError(t, err)
	assert.Equal(1, got.Summary.Attempts.Eligible)
	assert.InDelta(1, got.Summary.Cost.TotalUSD, 0.0001)
	assert.Equal(2, got.Summary.Reviews.Total, "attempt filters do not change logical reviews")

	exclusive, err := db.GetAnalytics(AnalyticsOptions{
		Since: base, Until: base.Add(2 * time.Hour), Bucket: AnalyticsBucketHour,
	})
	require.NoError(t, err)
	assert.Equal(1, exclusive.Summary.Reviews.Total, "until is exclusive")
	assert.Equal(1, exclusive.Summary.Attempts.Eligible)
	require.Len(t, exclusive.TimeSeries, 2)
	assert.Equal(0, exclusive.TimeSeries[0].Reviews.Total)
	assert.Equal(1, exclusive.TimeSeries[1].Reviews.Total)
}

type analyticsJobSeed struct {
	name       string
	jobType    string
	panelRole  string
	status     JobStatus
	source     string
	agent      string
	model      string
	enqueuedAt time.Time
	startedAt  time.Time
	finishedAt time.Time
	verdict    *int
	closed     bool
	invoked    bool
	costJSON   string
}

func seedAnalyticsJob(t *testing.T, db *DB, repo *Repo, seed analyticsJobSeed) *ReviewJob {
	t.Helper()
	commit := createCommit(t, db, repo.ID, "analytics-"+seed.name)
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: commit.SHA,
		Agent: seed.agent, Model: seed.model, JobType: seed.jobType,
		PanelRole: seed.panelRole, Source: seed.source,
	})
	require.NoError(t, err)
	var started, finished any
	if !seed.startedAt.IsZero() {
		started = seed.startedAt.UTC().Format("2006-01-02 15:04:05")
	}
	if !seed.finishedAt.IsZero() {
		finished = seed.finishedAt.UTC().Format("2006-01-02 15:04:05")
	}
	_, err = db.Exec(`UPDATE review_jobs SET status = ?, enqueued_at = ?, started_at = ?, finished_at = ?,
		agent_invoked = ?, source = ?, agent = ?, model = ? WHERE id = ?`,
		seed.status, seed.enqueuedAt.UTC().Format("2006-01-02 15:04:05"), started, finished,
		seed.invoked || seed.costJSON != "", seed.source, seed.agent, seed.model, job.ID)
	require.NoError(t, err)
	if seed.costJSON != "" {
		_, err = db.Exec(`UPDATE review_jobs SET token_usage = ? WHERE id = ?`, seed.costJSON, job.ID)
		require.NoError(t, err)
	}
	if seed.verdict != nil {
		_, err = db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, created_at, closed, verdict_bool)
			VALUES (?, ?, '', '', ?, ?, ?)`, job.ID, seed.agent,
			seed.finishedAt.UTC().Format("2006-01-02 15:04:05"), seed.closed, *seed.verdict)
		require.NoError(t, err)
	}
	return job
}

func TestAnalyticsBucketString(t *testing.T) {
	for _, bucket := range []AnalyticsBucket{
		AnalyticsBucketHour, AnalyticsBucketDay, AnalyticsBucketWeek, AnalyticsBucketMonth,
	} {
		assert.NotEmpty(t, fmt.Sprint(bucket))
	}
}
