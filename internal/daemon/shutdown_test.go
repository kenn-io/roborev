package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
