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

	// Discovery prefers the socket, so the key stays off TCP while it works.
	writeAuthClientConfig(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	discovered, err := GetAnyRunningDaemon()
	require.NoError(t, err)
	assert.Equal(t, unix, discovered.Endpoint())

	// Both published listeners accept the key and enforce it.
	for _, ep := range []DaemonEndpoint{unix, tcp} {
		writeAuthClientConfig(t, "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
		resp, err := ep.HTTPClient(time.Second).Get(ep.BaseURL() + "/api/ping")
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		writeAuthClientConfig(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
		ping, err := ProbeDaemon(ep, time.Second)
		require.NoError(t, err)
		assert.True(t, ping.OK)
	}

	// With the socket gone, discovery falls back to the published TCP endpoint.
	require.NoError(t, os.Remove(unix.Address))
	discovered, err = GetAnyRunningDaemon()
	require.NoError(t, err)
	assert.Equal(t, tcp, discovered.Endpoint())
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

func TestAuthKillDaemonFallsBackToTCPWhenSocketIsGone(t *testing.T) {
	testenv.SetDataDir(t)
	writeAuthClientConfig(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	shutdownAuth := make(chan string, 1)
	var tcpServer *http.Server
	tcpServer = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ping":
			fmt.Fprint(w, `{"ok":true,"service":"roborev"}`)
		case "/api/shutdown":
			shutdownAuth <- r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			go func() { _ = tcpServer.Close() }()
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = tcpServer.Serve(listener) }()
	t.Cleanup(func() { _ = tcpServer.Close() })

	// A cleaner removed the socket file while the daemon kept running.
	socketDir, err := os.MkdirTemp("", "rr-gone")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	info := &RuntimeInfo{
		Network:          "tcp",
		Address:          listener.Addr().String(),
		AlternateNetwork: "unix",
		AlternateAddress: filepath.Join(socketDir, "d.sock"),
	}
	require.Len(t, info.Endpoints(), 2, "the record must list the missing socket")
	require.NoError(t, KillDaemon(info))
	assert.Equal(t, "Bearer 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", <-shutdownAuth)
}
