package tui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

//nolint:paralleltest // This test changes the global config environment.
func TestAuthTUIQueriesAndStreaming(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "test-shared-key"`), 0o600))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-shared-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/stream/events" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte("{\"type\":\"review.closed\",\"job_id\":23}\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"worker_count":2,"queued_jobs":0}`))
	}))
	defer server.Close()
	m := newTuiModel(server.URL)
	assert.IsType(t, statusMsg{}, m.fetchStatus()())
	events := make(chan struct{}, 1)
	connected, err := sseReadLoop(context.Background(), testEndpointFromURL(server.URL), events)
	require.ErrorIs(t, err, io.EOF)
	assert.True(t, connected)
	assert.Len(t, events, 1)
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "wrong-key"`), 0o600))
	assert.IsType(t, statusErrMsg{}, m.fetchStatus()())
	connected, err = sseReadLoop(context.Background(), testEndpointFromURL(server.URL), events)
	require.Error(t, err)
	assert.False(t, connected)
}
