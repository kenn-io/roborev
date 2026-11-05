//go:build growth

package searchindex

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/growthtest"
)

func TestReconcilerInitialMirrorGrowth(t *testing.T) { //nolint:paralleltest // measures wall time
	var verifyMirror func()
	growthtest.Run(t, growthtest.Check{
		Small: 10000, Large: 100000,
		// Allow 2x timing variation above linear growth. Feed paging has its
		// own check because sidecar writes can hide repeated canonical scans.
		MaxRatio: 20,
		Floor:    float64(10 * time.Millisecond),
		Measure: func(run, reset func()) float64 {
			cost := growthtest.Fastest(3)(run, reset)
			verifyMirror()
			return cost
		},
		Build: func(tb testing.TB, size int) (func(), func()) {
			db := seedCanonicalReconcilerHistory(tb, size, 0)
			// A fresh install has no planner statistics.
			var statsTables int
			require.NoError(tb, db.QueryRowContext(tb.Context(), `SELECT count(*) FROM sqlite_schema WHERE name = 'sqlite_stat1'`).Scan(&statsTables))
			require.Zero(tb, statsTables)
			var index *Index
			var r *Reconciler
			verifyMirror = func() {
				health := r.Health()
				require.True(tb, health.MirrorComplete)
				require.EqualValues(tb, size, health.Indexed)
				var indexed int
				require.NoError(tb, index.db.QueryRowContext(tb.Context(), `SELECT count(*) FROM review_fts`).Scan(&indexed))
				require.Equal(tb, size, indexed)
			}
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
				}, func() {
					if index != nil {
						verifyMirror()
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
