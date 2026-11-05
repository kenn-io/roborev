//go:build growth

package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/growthtest"
)

func TestSearchFeedPagingGrowth(t *testing.T) { //nolint:paralleltest // measures wall time
	growthtest.Run(t, growthtest.Check{
		Small: 5000, Large: 50000,
		MaxRatio: 20,
		Floor:    float64(time.Millisecond),
		Measure:  growthtest.Fastest(3),
		Build: func(tb testing.TB, size int) (func(), func()) {
			db := openTestDB(t)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			repo, err := db.GetOrCreateRepo(tb.TempDir())
			require.NoError(tb, err)
			seedRangeReviews(tb.Context(), tb, db, repo.ID, size, "No issues found.")

			// Fresh installs have no ANALYZE statistics to guide SQLite.
			var statsTables int
			require.NoError(tb, db.QueryRowContext(tb.Context(), `SELECT count(*) FROM sqlite_schema WHERE name = 'sqlite_stat1'`).Scan(&statsTables))
			require.Zero(tb, statsTables)
			return func() {
				var cursor int64
				count := 0
				for {
					page, err := db.ListSearchDocuments(tb.Context(), cursor, 500)
					require.NoError(tb, err)
					if len(page) == 0 {
						break
					}
					count += len(page)
					cursor = page[len(page)-1].ReviewID
				}
				require.Equal(tb, size, count)
			}, nil
		},
	})
}
