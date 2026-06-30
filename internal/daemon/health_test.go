package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// setupTestServer creates a temporary DB and Server, handling cleanup automatically.
func setupTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(dir + "/test.db")
	require.NoError(t, err, "Failed to open database")
	t.Cleanup(func() { db.Close() })

	cfg := config.DefaultConfig()
	server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { server.Close() })
	return server
}

// executeHealthCheck sends a request to the health endpoint and returns the recorder.
func executeHealthCheck(server *Server, method string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/health", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	return w
}

// decodeHealthStatus parses a HealthStatus from the response body.
func decodeHealthStatus(t *testing.T, w *httptest.ResponseRecorder) storage.HealthStatus {
	t.Helper()
	var health storage.HealthStatus
	err := json.NewDecoder(w.Result().Body).Decode(&health)
	require.NoError(t, err, "Failed to parse health response")
	return health
}

func TestHealth(t *testing.T) {
	t.Run("Happy Path", func(t *testing.T) {
		server := setupTestServer(t)
		w := executeHealthCheck(server, http.MethodGet)
		assert.Equal(t, http.StatusOK, w.Code)

		health := decodeHealthStatus(t, w)
		assert.NotEmpty(t, health.Uptime, "Uptime")
		assert.NotEmpty(t, health.Version, "Version")
		assert.True(t, hasComponent(health.Components, "database"), "Expected component 'database' in health check")
		assert.True(t, hasComponent(health.Components, "workers"), "Expected component 'workers' in health check")
		assert.True(t, health.Healthy, "Expected health to be OK")
	})

	t.Run("With Errors", func(t *testing.T) {
		server := setupTestServer(t)
		// Log specific errors
		if server.errorLog != nil {
			server.errorLog.LogError("worker", "test error 1", 123)
			server.errorLog.LogError("worker", "test error 2", 456)
		}

		w := executeHealthCheck(server, http.MethodGet)
		health := decodeHealthStatus(t, w)

		assert.Positive(t, health.ErrorCount, "Expected error count > 0")
		assert.True(t, hasError(health.RecentErrors, "worker", 456), "Expected error for component 'worker' with JobID 456")
	})

	t.Run("Method Not Allowed", func(t *testing.T) {
		server := setupTestServer(t)
		w := executeHealthCheck(server, http.MethodPost)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	})
}

// Helpers

func hasComponent(components []storage.ComponentHealth, name string) bool {
	for _, c := range components {
		if c.Name == name {
			return true
		}
	}
	return false
}

func hasError(errors []storage.ErrorEntry, component string, jobID int64) bool {
	for _, e := range errors {
		if e.Component == component && e.JobID == jobID {
			return true
		}
	}
	return false
}
