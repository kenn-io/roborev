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
	cfg.AuthKey = "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.Web.Enabled = false
	server := NewServer(db, cfg, "")
	errCh, info := startServerAndWaitForRuntime(t, server)
	t.Cleanup(func() { stopTestServer(t, server, errCh) })
	require.Len(t, info.Endpoints(), 2)
	writeAuthClientConfig(t, "5edae9b7c0eda3b75e0d34d9eda9607f54b4ac60f7e02e593dec0b2b1e98a5e0")
	for _, endpoint := range info.Endpoints() {
		resp, err := endpoint.HTTPClient(time.Second).Get(endpoint.BaseURL() + "/api/ping")
		require.NoError(t, err)
		require.NotNil(t, resp)
		resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		writeAuthClientConfig(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
		ping, err := ProbeDaemon(endpoint, time.Second)
		require.NoError(t, err)
		assert.True(t, ping.OK)
		writeAuthClientConfig(t, "5edae9b7c0eda3b75e0d34d9eda9607f54b4ac60f7e02e593dec0b2b1e98a5e0")
	}
}
