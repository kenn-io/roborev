package telemetry_test

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

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
	for _, state := range []string{"", "1", "unexpected"} {
		t.Run("stored outage/"+state, func(t *testing.T) {
			t.Setenv(telemetry.EnabledEnv, "1")
			t.Setenv(telemetry.GenericEnabledEnv, "1")
			endpoint, messages := testutil.NewPostHogStub(t)
			testenv.SetDataDir(t)
			db := testutil.OpenTestDB(t)
			require.NoError(t, db.SetSyncState("telemetry.sync_outage", state))
			reporter, err := telemetry.NewReporter(telemetry.Options{Database: db, Endpoint: endpoint, DailyClaimsPath: filepath.Join(t.TempDir(), "daily.json")})
			require.NoError(t, err)
			synctest.Test(t, func(t *testing.T) {
				server := daemon.NewServer(db, config.DefaultConfig(), "")
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				server.SetTelemetry(reporter)
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
			want := 1
			if state != "" {
				want = 0
			}
			require.Len(t, sent, want)
			stored, err := db.GetSyncState("telemetry.sync_outage")
			require.NoError(t, err)
			if state != "" {
				assert.Equal(t, state, stored)
			}
			for _, message := range sent {
				assert.Equal(t, "sync_failed", message.Event)
				assert.Equal(t, "connect", message.Properties["reason"])
			}
		})
	}
}

func TestOutcomeDaemonSyncStateReadFailure(t *testing.T) {
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(telemetry.GenericEnabledEnv, "1")
	testenv.SetDataDir(t)
	endpoint, messages := testutil.NewPostHogStub(t)
	db := testutil.OpenTestDB(t)
	reporter, err := telemetry.NewReporter(telemetry.Options{Database: db, Endpoint: endpoint, DailyClaimsPath: filepath.Join(t.TempDir(), "daily.json")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	_, err = db.Exec("ALTER TABLE sync_state RENAME TO unavailable_sync_state")
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		server := daemon.NewServer(db, config.DefaultConfig(), "")
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		server.SetTelemetry(reporter)
		worker := storage.NewSyncWorker(db, config.SyncConfig{Enabled: true, Interval: "1h"})
		server.SetSyncWorker(worker)
		t.Cleanup(worker.Stop)
		require.NoError(t, worker.Start())
		synctest.Wait()
		_, err := worker.SyncNow()
		require.ErrorContains(t, err, "failed to connect")
	})
	require.NoError(t, reporter.Close())
	sent := messages()
	require.Len(t, sent, 1)
	assert.Equal(t, "sync_failed", sent[0].Event)
	assert.Equal(t, "connect", sent[0].Properties["reason"])
}

func TestIntegrationOutcomeDaemonSyncRecovery(t *testing.T) {
	pgURL := os.Getenv("TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("requires an isolated PostgreSQL database via TEST_POSTGRES_URL")
	}
	for _, recovery := range []string{"connect only", "manual", "background"} {
		t.Run(recovery, func(t *testing.T) {
			t.Setenv(telemetry.EnabledEnv, "1")
			t.Setenv(telemetry.GenericEnabledEnv, "1")
			testenv.SetDataDir(t)
			endpoint, messages := testutil.NewPostHogStub(t)
			db := testutil.OpenTestDB(t)
			cfg := config.SyncConfig{Enabled: true, Interval: "1h"}
			// Each phase replaces the daemon and worker but keeps their SQLite state.
			phase := func(cfg config.SyncConfig, background bool, interact func(*storage.SyncWorker)) {
				t.Helper()
				reporter, err := telemetry.NewReporter(telemetry.Options{Database: db, Endpoint: endpoint, DailyClaimsPath: filepath.Join(t.TempDir(), "daily.json")})
				require.NoError(t, err)
				server := daemon.NewServer(db, config.DefaultConfig(), "")
				server.SetTelemetry(reporter)
				worker := storage.NewSyncWorker(db, cfg)
				server.SetSyncWorker(worker)
				defer func() {
					worker.Stop()
					require.NoError(t, server.Close())
					require.NoError(t, reporter.Close())
				}()
				require.NoError(t, worker.SetSkipInitialSync(!background))
				require.NoError(t, worker.Start())
				interact(worker)
			}
			fail := func(worker *storage.SyncWorker) {
				for range 3 {
					_, err := worker.SyncNow()
					require.ErrorContains(t, err, "failed to connect")
				}
			}
			phase(cfg, false, fail)
			require.Len(t, messages(), 1)
			// Restart during the same outage and retry without a successful cycle.
			phase(cfg, false, fail)
			require.Len(t, messages(), 1)
			connected := cfg
			connected.PostgresURL = pgURL
			phase(connected, recovery == "background", func(worker *storage.SyncWorker) {
				// The external PostgreSQL connection cannot use synctest virtual time.
				require.Eventually(t, func() bool { healthy, _ := worker.HealthCheck(); return healthy }, 10*time.Second, 10*time.Millisecond)
				require.NoError(t, worker.FinalPush())
				switch recovery {
				case "manual":
					_, err := worker.SyncNow()
					require.NoError(t, err)
				case "background":
					// Wait for the background PostgreSQL push-and-pull cycle to finish.
					require.Eventually(t, func() bool {
						outage, err := db.GetSyncState("telemetry.sync_outage")
						return err == nil && outage == ""
					}, 10*time.Second, 10*time.Millisecond)
				}
			})
			require.Len(t, messages(), 1)
			phase(cfg, false, fail)
			sent := messages()
			want := 2
			if recovery == "connect only" {
				want = 1
			}
			require.Len(t, sent, want)
			for _, message := range sent {
				assert.Equal(t, "sync_failed", message.Event)
				assert.Equal(t, "connect", message.Properties["reason"])
			}
		})
	}
}
