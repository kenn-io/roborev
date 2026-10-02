package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestAuthShutdownDoesNotRemoveDeniedRuntimeWithoutPID(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	ep, err := ParseEndpoint(server.Listener.Addr().String())
	require.NoError(t, err)
	path := filepath.Join(config.DataDir(), "legacy-runtime.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"pid":0}`), 0o600))
	info := &RuntimeInfo{Network: ep.Network, Address: ep.Address, SourcePath: path}
	require.ErrorIs(t, KillDaemon(info), ErrDaemonAccessDenied)
	assert.FileExists(t, path)
}

func TestAuthShutdownReportsDeniedRequest(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	server := httptest.NewServer(newAuthTestServer(t, "test-shared-key").httpServer.Handler)
	defer server.Close()
	ep := authEndpoint(t, server.URL)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := requestGracefulDaemonShutdown(ctx, ep, func() bool { return false })
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
}
