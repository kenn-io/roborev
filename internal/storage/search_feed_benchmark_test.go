package storage

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkSearchFeedPagingScaling(b *testing.B) {
	var costs []float64
	for _, historySize := range []int{5000, 50000} {
		b.Run(fmt.Sprintf("reviews=%d", historySize), func(b *testing.B) {
			db, err := Open(filepath.Join(b.TempDir(), "reviews.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, db.Close()) })
			repo, err := db.GetOrCreateRepo(b.TempDir())
			require.NoError(b, err)
			_, err = db.ExecContext(b.Context(), `
				WITH RECURSIVE sequence(n) AS (
					SELECT 1 UNION ALL SELECT n + 1 FROM sequence WHERE n < ?
				)
				INSERT INTO review_jobs (repo_id, git_ref, agent, status, job_type, uuid)
				SELECT ?, 'base..head', 'test', 'done', 'range',
				       printf('00000000-0000-4000-8000-%012d', n)
				FROM sequence`, historySize, repo.ID)
			require.NoError(b, err)
			_, err = db.ExecContext(b.Context(), `
				INSERT INTO reviews (job_id, agent, output, uuid)
				SELECT id, agent, 'No issues found.', uuid FROM review_jobs`)
			require.NoError(b, err)

			// Fresh installs have no ANALYZE statistics to guide SQLite.
			var statsTables int
			require.NoError(b, db.QueryRowContext(b.Context(),
				`SELECT count(*) FROM sqlite_schema WHERE name = 'sqlite_stat1'`).Scan(&statsTables))
			require.Zero(b, statsTables)

			for b.Loop() {
				var cursor int64
				count := 0
				for {
					page, err := db.ListSearchDocuments(b.Context(), cursor, 500)
					require.NoError(b, err)
					if len(page) == 0 {
						break
					}
					count += len(page)
					cursor = page[len(page)-1].ReviewID
				}
				require.Equal(b, historySize, count)
			}
			costs = append(costs, float64(b.Elapsed())/float64(b.N))
		})
	}
	if len(costs) == 2 {
		// Ten times the history should cost about ten times as much. Allow
		// 2x timing noise while rejecting the quadratic 100x growth.
		ratio := costs[1] / costs[0]
		b.ReportMetric(ratio, "cost_ratio")
		require.Less(b, ratio, 20.0, "paging cost should grow linearly with review history")
	}
}
