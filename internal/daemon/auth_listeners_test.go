//go:build !windows

package daemon

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

func TestAuthStartupAndBothListeners(t *testing.T) {
	testenv.SetDataDir(t)
	setShortRuntimeDir(t)
	// Readiness must use the custom startup key, not the ordinary client config.
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = broken-secret`), 0o600))
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.AuthKey = "test-shared-key"
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.Web.Enabled = false
	server := NewServer(db, cfg, "")
	errCh, info := startServerAndWaitForRuntime(t, server)
	t.Cleanup(func() { stopTestServer(t, server, errCh) })
	require.Len(t, info.Endpoints(), 2)
	writeAuthClientConfig(t, "different-client-key")
	for _, endpoint := range info.Endpoints() {
		resp, err := endpoint.HTTPClient(time.Second).Get(endpoint.BaseURL() + "/api/ping")
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		writeAuthClientConfig(t, "test-shared-key")
		ping, err := ProbeDaemon(endpoint, time.Second)
		require.NoError(t, err)
		assert.True(t, ping.OK)
		writeAuthClientConfig(t, "different-client-key")
	}
}
