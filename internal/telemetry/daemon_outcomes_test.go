package telemetry_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	handler := server.Handler()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/telemetry/events", strings.NewReader(`{"event":"error_shown","properties":{"surface":"web","kind":"unknown","message":"omitted"}}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusAccepted, response.Code)
	assert.JSONEq(t, `{"status":"queued"}`, response.Body.String())
	require.NoError(t, reporter.Close())
	sent := messages()
	require.Len(t, sent, 3)
	for _, message := range sent[:2] {
		assert.Equal(t, "sync_failed", message.Event)
		assert.Equal(t, "connect", message.Properties["reason"])
	}
	assert.Equal(t, "error_shown", sent[2].Event)
	assert.Equal(t, "other", sent[2].Properties["kind"])
	assert.NotContains(t, sent[2].Properties, "message")
}
