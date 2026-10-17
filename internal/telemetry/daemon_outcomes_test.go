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
	server := daemon.NewServer(db, config.DefaultConfig(), "")
	t.Cleanup(func() { _ = server.Close() })
	reporter, err := telemetry.NewReporter(telemetry.Options{Database: db, Endpoint: endpoint, DailyClaimsPath: filepath.Join(t.TempDir(), "daily.json")})
	require.NoError(t, err)
	server.SetTelemetry(reporter)
	synctest.Test(t, func(t *testing.T) {
		worker := storage.NewSyncWorker(db, config.SyncConfig{Enabled: true, Interval: "1h"})
		server.SetSyncWorker(worker)
		require.NoError(t, worker.Start())
		synctest.Wait()
		_, err := worker.SyncNow()
		require.ErrorContains(t, err, "failed to connect")
		worker.Stop()
	})
	require.NoError(t, reporter.Close())
	sent := messages()
	require.Len(t, sent, 2)
	for _, message := range sent {
		assert.Equal(t, "sync_failed", message.Event)
		assert.Equal(t, "connect", message.Properties["reason"])
	}
}
