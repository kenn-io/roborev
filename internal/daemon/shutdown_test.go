package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
)

func TestShutdownEndpointSignalsGracefulShutdown(t *testing.T) {
	assert := assert.New(t)
	server := setupTestServer(t)

	select {
	case <-server.ShutdownRequested():
		require.Fail(t, "shutdown must not be requested before the endpoint is hit")
	default:
	}

	req := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code)
	var body struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(w.Result().Body).Decode(&body))
	assert.Equal("shutting down", body.Status)

	select {
	case <-server.ShutdownRequested():
		// Channel closed - shutdown signaled.
	default:
		assert.Fail("ShutdownRequested channel must be closed after POST /api/shutdown")
	}

	// A second request must not panic (close-once semantics).
	w = httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/shutdown", nil))
	assert.Equal(http.StatusOK, w.Code)
}

func TestShutdownEndpointRejectsGet(t *testing.T) {
	server := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/shutdown", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	select {
	case <-server.ShutdownRequested():
		assert.Fail(t, "GET must not trigger shutdown")
	default:
	}
}

func TestShutdownBlocksClaimsWithoutChangingQueuePause(t *testing.T) {
	server := setupTestServer(t)

	paused, err := server.db.IsQueuePaused()
	require.NoError(t, err)
	assert.False(t, paused)

	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(
		w, httptest.NewRequest(http.MethodPost, "/api/shutdown", nil),
	)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	draining, err := server.db.IsShutdownDraining()
	require.NoError(t, err)
	assert.True(t, draining)
	paused, err = server.db.IsQueuePaused()
	require.NoError(t, err)
	assert.False(t, paused)

	require.NoError(t, server.Stop())
	draining, err = server.db.IsShutdownDraining()
	require.NoError(t, err)
	assert.False(t, draining)
}

func TestShutdownRejectsQueueMutationsWhileDraining(t *testing.T) {
	server := setupTestServer(t)

	shutdown := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(
		shutdown, httptest.NewRequest(http.MethodPost, "/api/shutdown", nil),
	)
	require.Equal(t, http.StatusOK, shutdown.Code, shutdown.Body.String())

	unpause := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(
		unpause, httptest.NewRequest(http.MethodPost, "/api/queue/unpause", nil),
	)
	assert.Equal(t, http.StatusConflict, unpause.Code)
	draining, err := server.db.IsShutdownDraining()
	require.NoError(t, err)
	assert.True(t, draining)
}

func TestStopRetriesDrainPreparationAfterTransientFailure(t *testing.T) {
	server := setupTestServer(t)

	_, err := server.db.Exec(`DROP TABLE daemon_state`)
	require.NoError(t, err)
	require.ErrorContains(t, server.Stop(), "block job claims for shutdown")

	_, err = server.db.Exec(`
		CREATE TABLE daemon_state (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`)
	require.NoError(t, err)
	require.NoError(t, server.Stop())
}

func TestStopBoundsDrainStateCleanupWithSharedContext(t *testing.T) {
	testenv.SetDataDir(t)
	server := setupTestServer(t)
	require.NoError(t, WriteRuntime(
		DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"},
		nil,
		"test",
		nil,
	))

	originalTimeout := shutdownCleanupTimeout
	originalRetryInterval := shutdownCleanupRetryInterval
	shutdownCleanupTimeout = 30 * time.Millisecond
	shutdownCleanupRetryInterval = time.Millisecond
	t.Cleanup(func() {
		shutdownCleanupTimeout = originalTimeout
		shutdownCleanupRetryInterval = originalRetryInterval
	})

	server.workerPool.wg.Add(1)
	close(server.workerPool.readyCh)
	stopDone := make(chan error, 1)
	go func() { stopDone <- server.Stop() }()
	require.Eventually(t, func() bool {
		draining, err := server.db.IsShutdownDraining()
		return err == nil && draining
	}, time.Second, time.Millisecond)

	_, err := server.db.Exec(`DROP TABLE daemon_state`)
	require.NoError(t, err)
	server.workerPool.wg.Done()

	var stopErr error
	require.Eventually(t, func() bool {
		select {
		case stopErr = <-stopDone:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	require.ErrorContains(t, stopErr, "clear shutdown drain state")
	assert.NoFileExists(t, RuntimePath())
}

func TestServerStartClearsInterruptedShutdownDrain(t *testing.T) {
	testenv.SetDataDir(t)
	db, err := storage.Open(t.TempDir() + "/test.db")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.SetShutdownDraining(true))

	cfg := config.DefaultConfig()
	cfg.ServerAddr = "127.0.0.1:0"
	server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
	errCh, _ := startServerAndWaitForRuntime(t, server)

	draining, err := db.IsShutdownDraining()
	require.NoError(t, err)
	assert.False(t, draining)
	stopTestServer(t, server, errCh)
}

func TestStopKeepsRuntimePublishedUntilWorkersFinish(t *testing.T) {
	testenv.SetDataDir(t)
	server := setupTestServer(t)
	require.NoError(t, WriteRuntime(
		DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"},
		nil,
		"test",
		nil,
	))

	server.workerPool.wg.Add(1)
	close(server.workerPool.readyCh)
	stopDone := make(chan error, 1)
	go func() { stopDone <- server.Stop() }()

	require.Eventually(t, func() bool {
		draining, err := server.db.IsShutdownDraining()
		return err == nil && draining
	}, time.Second, time.Millisecond)
	assert.FileExists(t, RuntimePath())
	assert.Never(t, func() bool {
		select {
		case <-stopDone:
			return true
		default:
			return false
		}
	}, 20*time.Millisecond, time.Millisecond)

	server.workerPool.wg.Done()
	var stopErr error
	require.Eventually(t, func() bool {
		select {
		case stopErr = <-stopDone:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	require.NoError(t, stopErr)
	assert.NoFileExists(t, RuntimePath())
}

func TestStopKeepsBrowserAvailableUntilWorkersFinish(t *testing.T) {
	server := setupTestServer(t)
	server.allowWebCompilationStub = true
	web := config.DefaultConfig().Web
	web.Listen = "127.0.0.1:0"
	runtime, err := server.startBrowserServer(web)
	require.NoError(t, err)
	require.NotNil(t, runtime)

	server.workerPool.wg.Add(1)
	close(server.workerPool.readyCh)
	stopDone := make(chan error, 1)
	go func() { stopDone <- server.Stop() }()

	require.Eventually(t, func() bool {
		draining, drainErr := server.db.IsShutdownDraining()
		return drainErr == nil && draining
	}, time.Second, time.Millisecond)
	response, err := http.Get(runtime.Origin + "/api/ping")
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusOK, response.StatusCode)

	server.workerPool.wg.Done()
	var stopErr error
	require.Eventually(t, func() bool {
		select {
		case stopErr = <-stopDone:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	require.NoError(t, stopErr)
	assert.Eventually(t, func() bool {
		client := &http.Client{Timeout: 50 * time.Millisecond}
		response, requestErr := client.Get(runtime.Origin + "/api/ping")
		if response != nil {
			_ = response.Body.Close()
		}
		return requestErr != nil
	}, time.Second, 10*time.Millisecond)
}
