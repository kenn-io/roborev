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
	machineID, jobID := uuid.New(), uuid.New()
	require.NoError(t, pool.RegisterMachine(t.Context(), machineID, "setup-source"))
	repoID, err := pool.GetOrCreateRepo(t.Context(), "https://github.com/example/setup-sync.git")
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
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
	server, db, _ := newTestServer(t)
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
	stats, err := worker.SyncNow()
	require.NoError(t, err)
	assert.Equal(t, 1, stats.PulledJobs)
	assert.Equal(t, 1, stats.PulledReviews)
	assert.Equal(t, []string{"setup_finished"}, client.events)
	marker, err := db.GetSyncState(storage.SyncStateSetupFinished)
	require.NoError(t, err)
	assert.Equal(t, "1", marker)
}
