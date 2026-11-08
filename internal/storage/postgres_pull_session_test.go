//go:build postgres

package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegration_PullFailureKeepsCursorAndRetries(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	env := newIntegrationEnv(t)
	seedPullHistory(t, env.Pool, 101)
	db := openTestDB(t)
	defer db.Close()
	machineID, err := db.GetMachineID()
	require.NoError(t, err)
	_, pageCursor, err := env.Pool.PullJobs(t.Context(), machineID, "", 100)
	require.NoError(t, err)
	worker := NewSyncWorker(db, testSyncConfig())
	_, err = db.ExecContext(t.Context(), `CREATE TRIGGER fail_pull BEFORE INSERT ON review_jobs WHEN (SELECT count(*) FROM review_jobs) = 100 BEGIN SELECT RAISE(ABORT, 'injected job failure'); END`)
	require.NoError(t, err)
	_, err = worker.pullChangesWithStats(t.Context(), env.Pool)
	require.ErrorContains(t, err, "injected job failure")
	cursor, err := db.GetSyncState(SyncStateLastJobCursor)
	require.NoError(t, err)
	assert.Equal(t, pageCursor, cursor, "failed page must leave the preceding page cursor intact")
	var committed int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM review_jobs`).Scan(&committed))
	assert.Equal(t, 100, committed, "the committed first page survives the next page's failure")
	_, err = db.ExecContext(t.Context(), `DROP TRIGGER fail_pull`)
	require.NoError(t, err)
	_, err = worker.pullChangesWithStats(t.Context(), env.Pool)
	require.NoError(t, err)
	cursor, err = db.GetSyncState(SyncStateLastJobCursor)
	require.NoError(t, err)
	assert.NotEqual(t, pageCursor, cursor)
	for _, table := range []string{"review_jobs", "reviews", "responses"} {
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&count))
		assert.Equal(t, 101, count, table)
	}
}
