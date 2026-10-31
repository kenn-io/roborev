package storage

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func BenchmarkAggregateAnalyticsTimeSeries(b *testing.B) {
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		bucketCount int
		reviewCount int
	}{
		{name: "720-hourly-1-review", bucketCount: 720, reviewCount: 1},
		{name: "8760-hourly-1-review", bucketCount: 8760, reviewCount: 1},
		{name: "43824-hourly-1-review", bucketCount: 43824, reviewCount: 1},
		{name: "720-hourly-1000-reviews", bucketCount: 720, reviewCount: 1000},
		{name: "8760-hourly-10000-reviews", bucketCount: 8760, reviewCount: 10000},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			rows := make([]analyticsRow, tc.reviewCount)
			for i := range rows {
				rows[i] = analyticsRow{
					project: "synthetic-project", source: "synthetic-source",
					agent: "synthetic-agent", model: "synthetic-model",
					jobType: JobTypeReview, status: JobStatusDone,
					finishedAt:     base.Add(time.Duration(i%tc.bucketCount) * time.Hour),
					reviewDuration: 1.5, attemptDuration: 1,
					verdict: sql.NullInt64{Int64: 1, Valid: true}, eligible: true,
				}
			}
			opts := AnalyticsOptions{
				Since: base, Until: base.Add(time.Duration(tc.bucketCount) * time.Hour),
				Bucket: AnalyticsBucketHour,
			}

			b.ReportAllocs()
			var result *AnalyticsSnapshot
			for b.Loop() {
				var err error
				result, err = aggregateAnalytics(rows, opts)
				require.NoError(b, err)
			}
			require.NotNil(b, result)
		})
	}
}

// analyticsBenchmarkJobs and analyticsBenchmarkPrompt size the history the
// Analytics benchmarks share. Both tables carry prompt payloads comparable to
// real histories. Those payloads matter: the analytics columns are stored
// after review_jobs.prompt and reviews.prompt, so any query that reads them
// from the table instead of the analytics indexes walks each prompt's
// overflow pages.
const (
	analyticsBenchmarkJobs   = 5000
	analyticsBenchmarkPrompt = 32 << 10
)

// openAnalyticsBenchmarkDB returns an on-disk database holding 60 days of
// finished jobs and reviews that end at the returned time.
func openAnalyticsBenchmarkDB(b *testing.B) (*DB, time.Time) {
	const historyDays = 60
	db, err := Open(filepath.Join(b.TempDir(), "reviews.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, db.Close()) })

	until := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	prompt := strings.Repeat("p", analyticsBenchmarkPrompt)
	tx, err := db.Begin()
	require.NoError(b, err)
	repoIDs := make([]int64, 8)
	for i := range repoIDs {
		result, err := tx.Exec(`INSERT INTO repos (root_path, name) VALUES (?, ?)`,
			fmt.Sprintf("/synthetic/repo-%d", i), fmt.Sprintf("repo-%d", i))
		require.NoError(b, err)
		repoIDs[i], err = result.LastInsertId()
		require.NoError(b, err)
	}
	sources := []string{JobSourcePostCommit, JobSourceCI, JobSourceAutoDesign, ""}
	statuses := []JobStatus{JobStatusDone, JobStatusDone, JobStatusDone, JobStatusFailed, JobStatusSkipped}
	for i := range analyticsBenchmarkJobs {
		finished := until.Add(-time.Duration(i) * (historyDays * 24 * time.Hour / analyticsBenchmarkJobs))
		started := finished.Add(-90 * time.Second)
		result, err := tx.Exec(`INSERT INTO review_jobs
			(repo_id, git_ref, agent, model, status, enqueued_at, started_at, finished_at,
			 job_type, source, token_usage, agent_invoked)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
			repoIDs[i%len(repoIDs)], fmt.Sprintf("sha-%d", i),
			fmt.Sprintf("agent-%d", i%3), fmt.Sprintf("model-%d", i%5), statuses[i%len(statuses)],
			started.Add(-time.Minute).Format(time.RFC3339), started.Format(time.RFC3339),
			finished.Format(time.RFC3339), JobTypeReview, sources[i%len(sources)],
			fmt.Sprintf(`{"cost_usd":%d.25,"has_cost":true,"total_output_tokens":1200}`, i%4))
		require.NoError(b, err)
		jobID, err := result.LastInsertId()
		require.NoError(b, err)
		_, err = tx.Exec(`INSERT INTO job_content (job_id, prompt) VALUES (?, zstd_compress(?))`, jobID, prompt)
		require.NoError(b, err)
		_, err = tx.Exec(`INSERT INTO reviews (job_id, agent, output, closed, verdict_bool)
			VALUES (?, 'agent', '', ?, ?)`, jobID, i%7 == 0, i%2)
		require.NoError(b, err)
	}
	require.NoError(b, tx.Commit())
	return db, until
}

// BenchmarkGetAnalytics measures the complete storage path behind the
// Analytics view: the window query and the aggregation into a snapshot.
func BenchmarkGetAnalytics(b *testing.B) {
	db, until := openAnalyticsBenchmarkDB(b)
	cases := []struct {
		name string
		opts AnalyticsOptions
	}{
		{"30d-day", AnalyticsOptions{Since: until.AddDate(0, 0, -30), Until: until, Bucket: AnalyticsBucketDay}},
		{"30d-day-split-model", AnalyticsOptions{
			Since: until.AddDate(0, 0, -30), Until: until, Bucket: AnalyticsBucketDay, Split: AnalyticsSplitModel,
		}},
		{"30d-day-split-project", AnalyticsOptions{
			Since: until.AddDate(0, 0, -30), Until: until, Bucket: AnalyticsBucketDay, Split: AnalyticsSplitProject,
		}},
		{"30d-day-project", AnalyticsOptions{
			Since: until.AddDate(0, 0, -30), Until: until, Bucket: AnalyticsBucketDay, Projects: []string{"repo-1"},
		}},
		{"30d-day-agent-model", AnalyticsOptions{
			Since: until.AddDate(0, 0, -30), Until: until, Bucket: AnalyticsBucketDay,
			Agents: []string{"agent-1"}, Models: []string{"model-2"},
		}},
		{"empty", AnalyticsOptions{Since: until.Add(time.Hour), Until: until.Add(2 * time.Hour), Bucket: AnalyticsBucketHour}},
		{"1h-hour", AnalyticsOptions{Since: until.Add(-time.Hour), Until: until, Bucket: AnalyticsBucketHour}},
		{"48h-hour", AnalyticsOptions{Since: until.Add(-48 * time.Hour), Until: until, Bucket: AnalyticsBucketHour}},
		{"7d-day", AnalyticsOptions{Since: until.AddDate(0, 0, -7), Until: until, Bucket: AnalyticsBucketDay}},
		{"90d-week", AnalyticsOptions{Since: until.AddDate(0, 0, -90), Until: until, Bucket: AnalyticsBucketWeek}},
		{"all-month", AnalyticsOptions{Until: until, Bucket: AnalyticsBucketMonth}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, err := db.GetAnalytics(tc.opts)
				require.NoError(b, err)
			}
		})
	}
}

// BenchmarkCompleteJobWithAnalyticsHistory measures finishing a review job,
// which is when the job enters idx_review_jobs_analytics and its review
// enters idx_reviews_job_verdict. It tracks the write cost those indexes add.
func BenchmarkCompleteJobWithAnalyticsHistory(b *testing.B) {
	db, _ := openAnalyticsBenchmarkDB(b)
	repo, err := db.GetOrCreateRepo(b.TempDir())
	require.NoError(b, err)
	prompt := strings.Repeat("p", analyticsBenchmarkPrompt)
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		_, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "base..head", Agent: "test", Prompt: prompt})
		require.NoError(b, err)
		job, err := db.ClaimJob("bench-worker")
		require.NoError(b, err)
		b.StartTimer()
		require.NoError(b, db.CompleteJob(job.ID, "test", "No issues found."))
	}
}

// BenchmarkBuildAnalyticsIndexes measures building both analytics indexes
// over an existing history, which the first daemon start after an upgrade
// does during migration.
func BenchmarkBuildAnalyticsIndexes(b *testing.B) {
	db, _ := openAnalyticsBenchmarkDB(b)
	for b.Loop() {
		b.StopTimer()
		_, err := db.Exec(`DROP INDEX idx_review_jobs_analytics`)
		require.NoError(b, err)
		_, err = db.Exec(`DROP INDEX idx_reviews_job_verdict`)
		require.NoError(b, err)
		b.StartTimer()
		_, err = db.Exec(analyticsJobsIndexSQL())
		require.NoError(b, err)
		_, err = db.Exec(analyticsReviewsIndexSQL)
		require.NoError(b, err)
	}
}
