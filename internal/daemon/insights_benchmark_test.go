package daemon

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// Keep 2,000 jobs in the window while the older history grows.
func BenchmarkFetchInsightsReviews(b *testing.B) {
	for _, count := range []int{10_000, 100_000} {
		for _, shape := range []string{"2-percent-failing", "no-window-failures", "non-review-failures-first", "50-percent-failing", "unrelated-repo-failures"} {
			b.Run(fmt.Sprintf("%d/%s", count, shape), func(b *testing.B) {
				db, err := storage.Open(filepath.Join(b.TempDir(), "reviews.db"))
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, db.Close()) })
				since := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
				asOf := since.Add(48 * time.Hour)
				tx, err := db.BeginTx(b.Context(), nil)
				require.NoError(b, err)
				_, err = tx.ExecContext(b.Context(), `INSERT INTO repos (id, root_path, name) VALUES
					(1, '/synthetic/repo', 'repo'), (2, '/synthetic/other', 'other')`)
				require.NoError(b, err)
				outputs := []string{"- Medium - Synthetic finding", "No findings."}
				structured := []string{string(testutil.ReviewFixtureJSON(outputs[0])), string(testutil.ReviewFixtureJSON(outputs[1]))}
				for i := range count {
					finished := since.Add(-time.Duration(i+1) * time.Minute)
					if i < 2000 {
						finished = since.Add(time.Duration(2000-i) * time.Minute)
					}
					repoID, verdict, jobType := 1, 1, storage.JobTypeReview
					if i%50 == 0 {
						verdict = 0
					}
					switch shape {
					case "no-window-failures":
						if i < 2000 {
							verdict = 1
						}
					case "non-review-failures-first":
						if i < 1000 {
							verdict, jobType = 0, storage.JobTypeCompact
							if i%2 == 0 {
								jobType = storage.JobTypeGoalReview
							}
						}
					case "50-percent-failing":
						verdict = i % 2
					case "unrelated-repo-failures":
						if i >= 2000 {
							repoID, verdict = 2, 0
						}
					}
					stamp := finished.Format(time.RFC3339Nano)
					_, err = tx.ExecContext(b.Context(), `INSERT INTO review_jobs
						(id, repo_id, git_ref, branch, agent, status, job_type, enqueued_at, finished_at)
						VALUES (?, ?, 'base..head', 'main', 'test', 'done', ?, ?, ?)`, i+1, repoID, jobType, stamp, stamp)
					require.NoError(b, err)
					_, err = tx.ExecContext(b.Context(), `INSERT INTO reviews (job_id, agent, output, verdict_bool, structured_output)
						VALUES (?, 'test', ?, ?, ?)`, i+1, outputs[verdict], verdict, structured[verdict])
					require.NoError(b, err)
				}
				require.NoError(b, tx.Commit())
				server := &Server{db: db}
				want := 40
				switch shape {
				case "no-window-failures":
					want = 0
				case "non-review-failures-first":
					want = 20
				case "50-percent-failing":
					want = maxInsightsReviews
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					reviews, err := server.fetchInsightsReviews("/synthetic/repo", "main", since, asOf)
					require.NoError(b, err)
					require.Len(b, reviews, want)
				}
			})
		}
	}
}
