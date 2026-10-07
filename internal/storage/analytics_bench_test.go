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

// BenchmarkGetAnalytics measures the complete storage path behind the
// Analytics view against an on-disk database whose job and review rows carry
// prompt payloads comparable to real histories. Those payloads matter: the
// analytics columns are stored after review_jobs.prompt and reviews.prompt, so
// reading them from the table walks each prompt's overflow pages.
func BenchmarkGetAnalytics(b *testing.B) {
	const (
		jobs        = 5000
		promptBytes = 32 << 10
		historyDays = 60
	)
	db, err := Open(filepath.Join(b.TempDir(), "reviews.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, db.Close()) })

	until := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	prompt := strings.Repeat("p", promptBytes)
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
	for i := range jobs {
		finished := until.Add(-time.Duration(i) * historyDays * 24 * time.Hour / jobs)
		started := finished.Add(-90 * time.Second)
		result, err := tx.Exec(`INSERT INTO review_jobs
			(repo_id, git_ref, agent, model, status, enqueued_at, started_at, finished_at,
			 prompt, job_type, source, token_usage, agent_invoked)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
			repoIDs[i%len(repoIDs)], fmt.Sprintf("sha-%d", i),
			fmt.Sprintf("agent-%d", i%3), fmt.Sprintf("model-%d", i%5), statuses[i%len(statuses)],
			started.Add(-time.Minute).Format(time.RFC3339), started.Format(time.RFC3339),
			finished.Format(time.RFC3339), prompt, JobTypeReview, sources[i%len(sources)],
			fmt.Sprintf(`{"cost_usd":%d.25,"has_cost":true,"total_output_tokens":1200}`, i%4))
		require.NoError(b, err)
		jobID, err := result.LastInsertId()
		require.NoError(b, err)
		_, err = tx.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, closed, verdict_bool)
			VALUES (?, 'agent', ?, '', ?, ?)`, jobID, prompt, i%7 == 0, i%2)
		require.NoError(b, err)
	}
	require.NoError(b, tx.Commit())

	cases := []struct {
		name string
		opts AnalyticsOptions
	}{
		{"30d-day", AnalyticsOptions{Since: until.AddDate(0, 0, -30), Until: until, Bucket: AnalyticsBucketDay}},
		{"30d-day-split-model", AnalyticsOptions{
			Since: until.AddDate(0, 0, -30), Until: until, Bucket: AnalyticsBucketDay, Split: AnalyticsSplitModel,
		}},
		{"30d-day-project", AnalyticsOptions{
			Since: until.AddDate(0, 0, -30), Until: until, Bucket: AnalyticsBucketDay, Projects: []string{"repo-1"},
		}},
		{"48h-hour", AnalyticsOptions{Since: until.Add(-48 * time.Hour), Until: until, Bucket: AnalyticsBucketHour}},
		{"all-month", AnalyticsOptions{Until: until, Bucket: AnalyticsBucketMonth}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var result *AnalyticsSnapshot
			for b.Loop() {
				result, err = db.GetAnalytics(tc.opts)
				require.NoError(b, err)
			}
			require.NotZero(b, result.Summary.Reviews.Total)
		})
	}
}
