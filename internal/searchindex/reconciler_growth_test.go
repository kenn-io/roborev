package searchindex

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/growthtest"
)

func TestReconcilerInitialMirrorGrowth(t *testing.T) { //nolint:paralleltest // measures wall time
	growthtest.Run(t, growthtest.Check{
		Small: 10000, Large: 100000,
		// Full builds measured 10.4x with cursor seeks and 20.6x without them.
		MaxRatio: 16,
		Floor:    float64(10 * time.Millisecond),
		Measure:  growthtest.Fastest(3),
		Build: func(tb testing.TB, size int) (func(), func()) {
			db := seedCanonicalReconcilerHistory(tb, size, 0)
			// A fresh install has no planner statistics.
			var statsTables int
			require.NoError(tb, db.QueryRowContext(tb.Context(), `SELECT count(*) FROM sqlite_schema WHERE name = 'sqlite_stat1'`).Scan(&statsTables))
			require.Zero(tb, statsTables)
			var index *Index
			var r *Reconciler
			tb.Cleanup(func() {
				if index != nil {
					require.NoError(tb, index.Close())
				}
			})
			return func() {
					for more := true; more; {
						var err error
						more, err = r.reconcileTurn(tb.Context())
						require.NoError(tb, err)
					}
					health := r.Health()
					require.True(tb, health.MirrorComplete)
					require.EqualValues(tb, size, health.Indexed)
					var indexed int
					require.NoError(tb, index.db.QueryRowContext(tb.Context(), `SELECT count(*) FROM review_fts`).Scan(&indexed))
					require.Equal(tb, size, indexed)
				}, func() {
					if index != nil {
						require.NoError(tb, index.Close())
					}
					var err error
					index, err = Open(tb.Context(), filepath.Join(tb.TempDir(), "reviews.search.db"))
					require.NoError(tb, err)
					r = NewReconciler(db, index, nil, ReconcilerConfig{})
				}
		},
	})
}
