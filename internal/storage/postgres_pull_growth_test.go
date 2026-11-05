//go:build postgres

package storage

import (
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/growthtest"

	"go.kenn.io/roborev/internal/config"
)

func TestIntegration_FirstPullGrowth(t *testing.T) { //nolint:paralleltest // measures wall time and shares the PostgreSQL schema
	growthtest.Run(t, growthtest.Check{
		Small: 1000, Large: 10000,
		MaxRatio: 20,
		Floor:    float64(100 * time.Millisecond),
		Measure:  growthtest.Fastest(3),
		Build: func(_ testing.TB, size int) (func(), func()) {
			env := newIntegrationEnv(t)
			seedPullHistory(t, env.Pool, size)
			cursors := make(map[string]string)
			for table, key := range map[string]string{"review_jobs": SyncStateLastJobCursor, "reviews": SyncStateLastReviewCursor} {
				var updated time.Time
				var id int64
				require.NoError(t, env.Pool.pool.QueryRow(t.Context(), "SELECT updated_at, id FROM "+table+" ORDER BY updated_at DESC, id DESC LIMIT 1").Scan(&updated, &id))
				cursors[key] = formatTimestampIDCursor(updated, id)
			}
			var db *DB
			var worker *SyncWorker
			t.Cleanup(func() {
				if db != nil {
					require.NoError(t, db.Close())
				}
			})
			return func() {
					stats, err := worker.pullChangesWithStats(t.Context(), env.Pool)
					require.NoError(t, err)
					require.Equal(t, size, stats.Jobs)
					require.Equal(t, size, stats.Reviews)
					for _, table := range []string{"review_jobs", "reviews"} {
						var count int
						require.NoError(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&count))
						require.Equal(t, size, count, table)
					}
					for key, expected := range cursors {
						cursor, err := db.GetSyncState(key)
						require.NoError(t, err)
						require.Equal(t, expected, cursor, key)
					}
				}, func() {
					if db != nil {
						require.NoError(t, db.Close())
					}
					db = openTestDB(t)
					worker = NewSyncWorker(db, config.SyncConfig{})
				}
		},
	})
}

// PostgreSQL paging gets its own bound because SQLite writes dominate the
// complete pull and can hide repeated scans of earlier remote pages.
func TestIntegration_PullPagingGrowth(t *testing.T) { //nolint:paralleltest // measures wall time and shares the PostgreSQL schema
	growthtest.Run(t, growthtest.Check{
		Small: 1000, Large: 10000,
		MaxRatio: 20,
		Floor:    float64(time.Millisecond),
		Measure:  growthtest.Fastest(3),
		Build: func(_ testing.TB, size int) (func(), func()) {
			pool := newIntegrationEnv(t).Pool
			seedPullHistory(t, pool, size)
			machineID := uuid.New()
			known := make(map[uuid.UUID]struct{}, size)
			rows, err := pool.pool.Query(t.Context(), "SELECT uuid FROM review_jobs")
			require.NoError(t, err)
			for rows.Next() {
				var id uuid.UUID
				require.NoError(t, rows.Scan(&id))
				known[id] = struct{}{}
			}
			rows.Close()
			require.NoError(t, rows.Err())
			require.Len(t, known, size)
			return func() {
				cursor, jobs := "", 0
				for {
					page, next, err := pool.PullJobs(t.Context(), machineID, cursor, 100)
					require.NoError(t, err)
					jobs += len(page)
					if len(page) < 100 {
						break
					}
					require.NotEqual(t, cursor, next)
					cursor = next
				}
				require.Equal(t, size, jobs)
				cursor, reviews := "", 0
				for {
					page, next, err := pool.PullReviews(t.Context(), machineID, known, cursor, 100)
					require.NoError(t, err)
					reviews += len(page)
					if len(page) < 100 {
						break
					}
					require.NotEqual(t, cursor, next)
					cursor = next
				}
				require.Equal(t, size, reviews)
			}, nil
		},
	})
}
