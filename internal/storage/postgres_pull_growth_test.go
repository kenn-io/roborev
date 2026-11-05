//go:build postgres

package storage

import (
	"context"
	"fmt"
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
			var db *DB
			var worker *SyncWorker
			t.Cleanup(func() {
				if db != nil {
					require.NoError(t, db.Close())
				}
			})
			return func() {
					_, err := worker.pullChangesWithStats(t.Context(), env.Pool)
					require.NoError(t, err)
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

// Count executed PostgreSQL work across every page of a first pull. This
// isolates repeated cursor scans from the SQLite writes in the full pull.
func TestIntegration_FirstPullQueryGrowth(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	var cost float64
	growthtest.Run(t, growthtest.Check{
		Small: 1000, Large: 10000,
		MaxRatio: 20,
		Floor:    1000,
		Measure: func(run, _ func()) float64 {
			run()
			return cost
		},
		Build: func(_ testing.TB, size int) (func(), func()) {
			pool := newIntegrationEnv(t).Pool
			seedPullHistory(t, pool, size)
			ctx := t.Context()
			tx, err := pool.pool.Begin(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })
			_, err = tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan")
			require.NoError(t, err)
			probes := []struct {
				name, table, column, query string
				cursors                    []string
			}{
				{name: "jobs", table: "review_jobs", column: "updated_at", query: pgPullJobsQuery},
				{name: "reviews", table: "reviews", column: "updated_at", query: pgPullReviewsQuery},
				{name: "responses", table: "responses", column: "inserted_at", query: pgPullResponsesQuery},
				{name: "assignments", table: "experiment_assignments", column: "inserted_at", query: pgPullExperimentAssignmentsQuery},
			}
			for i := range probes {
				probe := &probes[i]
				_, err := tx.Exec(ctx, "PREPARE growth_"+probe.name+" AS "+probe.query)
				require.NoError(t, err)
				probe.cursors = []string{"'1970-01-01T00:00:00Z', 0"}
				rows, err := tx.Query(ctx, fmt.Sprintf("SELECT %s, id FROM %s ORDER BY %s, id", probe.column, probe.table, probe.column))
				require.NoError(t, err)
				count := 0
				for rows.Next() {
					var updated time.Time
					var id int64
					require.NoError(t, rows.Scan(&updated, &id))
					count++
					if count%100 == 0 && count < size {
						probe.cursors = append(probe.cursors, fmt.Sprintf("'%s', %d", updated.Format(time.RFC3339Nano), id))
					}
				}
				rows.Close()
				require.NoError(t, rows.Err())
				require.Equal(t, size, count, probe.name)
			}
			machineID := uuid.New()
			return func() {
				defer func() { require.NoError(t, tx.Rollback(context.WithoutCancel(ctx))) }()
				var totalWork float64
				for _, probe := range probes {
					var returned float64
					for _, cursor := range probe.cursors {
						query := fmt.Sprintf("EXPLAIN (ANALYZE, TIMING OFF, FORMAT JSON) EXECUTE growth_%s('%s', %s, 100)", probe.name, machineID, cursor)
						var raw []byte
						require.NoError(t, tx.QueryRow(ctx, query).Scan(&raw))
						rows, work := pullPlanWork(t, raw)
						require.InDelta(t, 100, rows, 0, probe.name)
						returned += rows
						totalWork += work
					}
					require.InDelta(t, size, returned, 0, probe.name)
				}
				cost = totalWork
			}, nil
		},
	})
}
