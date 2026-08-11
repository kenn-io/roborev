package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestShutdownPausesClaimsAndRestoresQueueStateAfterStop(t *testing.T) {
	server := setupTestServer(t)

	paused, err := server.db.IsQueuePaused()
	require.NoError(t, err)
	assert.False(t, paused)

	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(
		w, httptest.NewRequest(http.MethodPost, "/api/shutdown", nil),
	)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	paused, err = server.db.IsQueuePaused()
	require.NoError(t, err)
	assert.True(t, paused)

	require.NoError(t, server.Stop())
	paused, err = server.db.IsQueuePaused()
	require.NoError(t, err)
	assert.False(t, paused)
}

func TestStopKeepsRuntimePublishedUntilWorkersFinish(t *testing.T) {
	testenv.SetDataDir(t)
	server := setupTestServer(t)
	require.NoError(t, WriteRuntime(
		DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"},
		nil,
		"test",
	))

	server.workerPool.wg.Add(1)
	close(server.workerPool.readyCh)
	stopDone := make(chan error, 1)
	go func() { stopDone <- server.Stop() }()

	require.Eventually(t, func() bool {
		paused, err := server.db.IsQueuePaused()
		return err == nil && paused
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
