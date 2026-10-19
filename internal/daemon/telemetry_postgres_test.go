//go:build postgres

package daemon

import (
	"os"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/testutil/setupclaim"
)

func TestIntegration_SetupFinishedAfterSyncPull(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	postgresURL := os.Getenv("TEST_POSTGRES_URL")
	if postgresURL == "" {
		postgresURL = "postgres://roborev_test:roborev_test_password@localhost:5433/roborev_test"
	}
	pool, err := storage.NewPgPool(t.Context(), postgresURL, storage.DefaultPgPoolConfig())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Pool().Exec(t.Context(), "DROP SCHEMA IF EXISTS roborev CASCADE")
	require.NoError(t, err)
	require.NoError(t, pool.EnsureSchema(t.Context()))
	machineID := uuid.New()
	require.NoError(t, pool.RegisterMachine(t.Context(), machineID, "setup-source"))
	repoID, err := pool.GetOrCreateRepo(t.Context(), "https://github.com/example/setup-sync.git")
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	for range 500 {
		jobID := uuid.New()
		require.NoError(t, pool.UpsertJob(t.Context(), storage.SyncableJob{
			UUID: jobID, GitRef: "main", Agent: "test", JobType: storage.JobTypeReview,
			ReviewType: "code", Status: "done", EnqueuedAt: now, FinishedAt: &now,
			SourceMachineID: machineID,
		}, repoID, nil))
		require.NoError(t, pool.UpsertReview(t.Context(), storage.SyncableReview{
			UUID: uuid.New(), JobUUID: jobID, JobEnqueuedAt: now, JobSourceMachineID: machineID,
			Agent: "test", StructuredOutput: testutil.ReviewFixtureJSON("No issues found."),
			UpdatedByMachineID: machineID, CreatedAt: now, UpdatedAt: now,
		}))
	}
	server, db, tmpDir := newTestServer(t)
	reconciler := newRecordingSearchReconciler()
	server.searchReconciler = reconciler
	claimAttempts := setupclaim.InstallCounter(t, db.DB)
	var path string
	require.NoError(t, db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path))
	claimDB, err := storage.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, claimDB.Close()) })
	claimDB.SetMaxOpenConns(1)
	pending, err := claimDB.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = pending.Close() }()
	server.db = claimDB
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)
	worker := storage.NewSyncWorker(db, config.SyncConfig{
		Enabled: true, PostgresURL: postgresURL, MachineName: "setup-target", Interval: "1h",
	})
	server.SetSyncWorker(worker)
	require.NoError(t, worker.SetSkipInitialSync(true))
	require.NoError(t, worker.Start())
	t.Cleanup(worker.Stop)
	// Wall-clock wait: the worker opens a real PostgreSQL connection.
	require.Eventually(t, func() bool {
		healthy, _ := worker.HealthCheck()
		return healthy
	}, 5*time.Second, time.Millisecond)
	type result struct {
		stats *storage.SyncStats
		err   error
	}
	returned := make(chan result, 1)
	go func() { stats, err := worker.SyncNow(); returned <- result{stats, err} }()
	// Wall-clock waits: SyncNow pulls PostgreSQL rows and setup writes SQLite.
	// The setup check starts only after the whole pull, whose duration depends
	// on the machine and -race, so only the test deadline bounds it.
	pullWait := 10 * time.Minute
	if deadline, ok := t.Deadline(); ok {
		pullWait = time.Until(deadline)
	}
	require.Eventually(t, func() bool {
		return claimDB.Stats().WaitCount == 1
	}, pullWait, time.Millisecond)
	var got result
	require.Eventually(t, func() bool {
		select {
		case got = <-returned:
			return true
		default:
			return false
		}
	}, 5*time.Second, time.Millisecond, "SyncNow must return while setup database work is pending")
	require.NoError(t, got.err)
	assert.Equal(t, 500, got.stats.PulledJobs)
	assert.Equal(t, 500, got.stats.PulledReviews)
	assert.NotEmpty(t, reconciler.wakes, "SyncNow must wake search after committed pulls")
	assert.Zero(t, claimDB.Stats().WaitDuration, "the original setup check must still be waiting for its connection")
	require.NoError(t, pending.Close())
	waitSetupFinished(t, client)
	assert.Equal(t, []string{"setup_finished"}, client.recordedEvents())
	marker, err := db.GetSyncState(storage.SyncStateSetupFinished)
	require.NoError(t, err)
	assert.Equal(t, "1", marker)
	_, err = worker.SyncNow()
	require.NoError(t, err)
	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "local", Agent: "test"})
	require.NoError(t, err)
	_, err = db.ClaimJob("worker")
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "", "No issues found."))
	server.workerPool.captureSetupFinished()
	require.NoError(t, server.Close())
	restarted := newServerWithLogs(db, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.SetTelemetry(client)
	restarted.captureSetupFinishedTelemetry()
	// Wall-clock wait: restart loads the marker from SQLite.
	require.Eventually(t, restarted.setupFinishedClaimed.Load, time.Second, time.Millisecond)
	restarted.workerPool.captureSetupFinished()
	assert.Equal(t, 1, claimAttempts())
	assert.Equal(t, []string{"setup_finished"}, client.recordedEvents())
}
