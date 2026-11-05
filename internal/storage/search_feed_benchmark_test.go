package storage

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkSearchFeedPagingScaling(b *testing.B) {
	costs := make(map[int]float64)
	for _, historySize := range []int{5000, 50000} {
		b.Run(fmt.Sprintf("reviews=%d", historySize), func(b *testing.B) {
			db, err := Open(filepath.Join(b.TempDir(), "reviews.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, db.Close()) })
			repo, err := db.GetOrCreateRepo(b.TempDir())
			require.NoError(b, err)
			seedRangeReviews(b.Context(), b, db, repo.ID, historySize, "No issues found.")

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
			costs[historySize] = float64(b.Elapsed()) / float64(b.N)
			small, haveSmall := costs[5000]
			large, haveLarge := costs[50000]
			if haveSmall && haveLarge {
				// Ten times the history should cost about ten times as much. Allow
				// 2x timing noise while rejecting the observed ~30x growth without +.
				ratio := large / small
				require.Less(b, ratio, 20.0, "paging cost should grow linearly with review history; measured cost ratio: %.2f", ratio)
			}
		})
	}
}
