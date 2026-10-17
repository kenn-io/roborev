package telemetry_test

import (
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

// The daemon suite disables telemetry for the process. This integration lives
// beside the reporter tests so it can verify actual queued events.
func TestOutcomeDaemonSyncFailure(t *testing.T) {
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(telemetry.GenericEnabledEnv, "1")
	endpoint, messages := testutil.NewPostHogStub(t)
	testenv.SetDataDir(t)
	db := testutil.OpenTestDB(t)
	reporter, err := telemetry.NewReporter(telemetry.Options{Database: db, Endpoint: endpoint, DailyClaimsPath: filepath.Join(t.TempDir(), "daily.json")})
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		server := daemon.NewServer(db, config.DefaultConfig(), "")
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		server.SetTelemetry(reporter)
		worker := storage.NewSyncWorker(db, config.SyncConfig{Enabled: true, Interval: "1h"})
		server.SetSyncWorker(worker)
		require.NoError(t, worker.SetSkipInitialSync(true))
		require.NoError(t, worker.Start())
		t.Cleanup(worker.Stop)
		_, err := worker.SyncNow()
		require.ErrorContains(t, err, "failed to connect")
	})
	require.NoError(t, reporter.Close())
	sent := messages()
	require.Len(t, sent, 1)
	assert.Equal(t, "sync_failed", sent[0].Event)
	assert.Equal(t, "connect", sent[0].Properties["reason"])
}
