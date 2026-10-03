package tui

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
)

//nolint:paralleltest // This test changes the global config environment.
func TestAuthTUIQueriesAndStreaming(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"`), 0o600))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer 51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015" {
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
	ep, err := daemon.ParseEndpoint(strings.TrimPrefix(server.URL, "https://"))
	require.NoError(t, err)
	ep.TLSCertPEM = string(pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
	}))
	require.NoError(t, daemon.WriteRuntimeWithTLS(ep, nil, "test-version", nil, ep.TLSCertPEM))
	m := newModel(ep, withExternalIODisabled())
	assert.IsType(t, statusMsg{}, m.fetchStatus()())
	events := make(chan struct{}, 1)
	connected, err := sseReadLoop(context.Background(), ep, events)
	require.ErrorIs(t, err, io.EOF)
	assert.True(t, connected)
	assert.Len(t, events, 1)
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "5edae9b7c0eda3b75e0d34d9eda9607f54b4ac60f7e02e593dec0b2b1e98a5e0"`), 0o600))
	assert.IsType(t, statusErrMsg{}, m.fetchStatus()())
	connected, err = sseReadLoop(context.Background(), ep, events)
	require.Error(t, err)
	assert.False(t, connected)
}
