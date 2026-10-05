//go:build !windows

package daemon

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
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
	cfg.AuthKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.Web.Enabled = false
	server := NewServer(db, cfg, "")
	errCh, info := startServerAndWaitForRuntime(t, server)
	t.Cleanup(func() { stopTestServer(t, server, errCh) })
	endpoints := info.Endpoints()
	require.Len(t, endpoints, 2)
	tcp, unix := endpoints[0], endpoints[1]
	require.Equal(t, "tcp", tcp.Network)
	require.Equal(t, "unix", unix.Network)

	// Without an opt-in, clients keep the key off TCP and discovery uses the socket.
	writeAuthClientConfig(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	resp, err := tcp.HTTPClient(time.Second).Get(tcp.BaseURL() + "/api/ping")
	assert.Nil(t, resp)
	require.ErrorIs(t, err, ErrPlaintextAuthTransport)
	discovered, err := GetAnyRunningDaemon()
	require.NoError(t, err)
	assert.Equal(t, unix, discovered.Endpoint())

	// Both listeners enforce the key. The TCP check uses the insecure opt-in.
	for _, tc := range []struct {
		endpoint DaemonEndpoint
		write    func(*testing.T, string)
	}{
		{endpoint: unix, write: writeAuthClientConfig},
		{endpoint: tcp, write: writeTCPAuthClientConfig},
	} {
		tc.write(t, "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
		resp, err := tc.endpoint.HTTPClient(time.Second).Get(tc.endpoint.BaseURL() + "/api/ping")
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		tc.write(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
		ping, err := ProbeDaemon(tc.endpoint, time.Second)
		require.NoError(t, err)
		assert.True(t, ping.OK)
	}
}

func TestAuthKillDaemonStopsThroughUnixSocket(t *testing.T) {
	testenv.SetDataDir(t)
	writeAuthClientConfig(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	var tcpRequests atomic.Int32
	tcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tcpRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer tcp.Close()

	socketDir, err := os.MkdirTemp("", "rr-kill")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "d.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	shutdownAuth := make(chan string, 1)
	var unixServer *http.Server
	unixServer = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ping":
			fmt.Fprint(w, `{"ok":true,"service":"roborev"}`)
		case "/api/shutdown":
			shutdownAuth <- r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			go func() { _ = unixServer.Close() }()
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = unixServer.Serve(listener) }()
	t.Cleanup(func() { _ = unixServer.Close() })

	// A daemon started before auth_key was set may only accept plain TCP,
	// but its runtime record also lists its private Unix socket.
	info := &RuntimeInfo{
		Network:          "tcp",
		Address:          tcp.Listener.Addr().String(),
		AlternateNetwork: "unix",
		AlternateAddress: socketPath,
	}
	require.NoError(t, KillDaemon(info))
	assert.Equal(t, "Bearer 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", <-shutdownAuth)
	assert.Zero(t, tcpRequests.Load())
}
