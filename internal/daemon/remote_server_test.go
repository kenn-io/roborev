package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

// allowLoopbackRemoteListen lets the remote listener bind loopback, which
// stands in for a Tailscale address in tests.
func allowLoopbackRemoteListen(t *testing.T) {
	t.Helper()
	previous := remoteListenAddrAllowed
	remoteListenAddrAllowed = func(addr netip.Addr) bool { return addr.IsLoopback() }
	t.Cleanup(func() { remoteListenAddrAllowed = previous })
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := probe.Addr().String()
	require.NoError(t, probe.Close())
	return addr
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

func TestStartRemoteServerServesAuthenticatedRequests(t *testing.T) {
	assert := assert.New(t)
	allowLoopbackRemoteListen(t)
	server, _, _ := newTestServer(t)
	server.remoteWhois = fakeWhois(RemoteAccessRead, nil)
	addr := freeLoopbackAddr(t)

	require.NoError(t, server.startRemoteServer(config.RemoteAPIConfig{Enabled: true, Listen: addr}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(server.remoteServer.Shutdown(ctx))
	})

	assert.Equal(http.StatusOK, getStatus(t, fmt.Sprintf("http://%s/api/ping", addr)))
	// The core mux serves this route; the remote handler refuses it.
	assert.Equal(http.StatusForbidden, getStatus(t, fmt.Sprintf("http://%s/api/sync/status", addr)))

	assert.Equal(5*time.Second, server.remoteServer.ReadHeaderTimeout)
	assert.Zero(server.remoteServer.ReadTimeout)
	assert.Zero(server.remoteServer.WriteTimeout)
}

func TestStartRemoteServerRefusesNonTailscaleAddress(t *testing.T) {
	server, _, _ := newTestServer(t)
	addr := freeLoopbackAddr(t)

	err := server.startRemoteServer(config.RemoteAPIConfig{Enabled: true, Listen: addr})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a Tailscale address")
	assert.Nil(t, server.remoteServer)

	// Nothing holds the port.
	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	assert.NoError(t, listener.Close())
}

func TestStartRemoteServerDisabled(t *testing.T) {
	server, _, _ := newTestServer(t)
	require.NoError(t, server.startRemoteServer(config.RemoteAPIConfig{}))
	assert.Nil(t, server.remoteServer)
}

func newRemoteTestDaemon(t *testing.T, remoteListen string) *Server {
	t.Helper()
	allowLoopbackRemoteListen(t)
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.Web.Listen = "127.0.0.1:0"
	cfg.RemoteAPI = config.RemoteAPIConfig{Enabled: true, Listen: remoteListen}
	server := NewServer(db, cfg, "", withWebCompilationStub())
	server.remoteWhois = fakeWhois(RemoteAccessRead, nil)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	return server
}

func TestServerStartAndStopRemoteListener(t *testing.T) {
	remoteAddr := freeLoopbackAddr(t)
	server := newRemoteTestDaemon(t, remoteAddr)

	errCh, _ := startServerAndWaitForRuntime(t, server)
	assert.Equal(t, http.StatusOK, getStatus(t, "http://"+remoteAddr+"/api/ping"))

	stopTestServer(t, server, errCh)
	listener, err := net.Listen("tcp", remoteAddr)
	require.NoError(t, err)
	assert.NoError(t, listener.Close())
}

func TestServerStartRemoteBindFailureClosesListeners(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, occupied.Close()) })
	server := newRemoteTestDaemon(t, occupied.Addr().String())

	err = server.Start(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen on remote_api.listen")
	assert.NoFileExists(t, RuntimePath())

	server.endpointMu.Lock()
	coreAddr := server.endpoint.Address
	server.endpointMu.Unlock()
	server.browserMu.Lock()
	browserListener := server.browserListener
	server.browserMu.Unlock()
	require.NotEmpty(t, coreAddr)
	require.NotNil(t, browserListener)

	client := &http.Client{Timeout: time.Second}
	_, err = client.Get("http://" + coreAddr + "/api/ping")
	require.Error(t, err)
	_, err = client.Get("http://" + browserListener.Addr().String() + "/api/ping")
	assert.Error(t, err)
}
