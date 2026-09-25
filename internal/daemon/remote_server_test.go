package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestStartRemoteServerServesAuthenticatedRequests(t *testing.T) {
	server, _, _ := newTestServer(t)
	server.remoteWhois = fakeWhois(RemoteAccessRead, nil)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := probe.Addr().String()
	require.NoError(t, probe.Close())

	// startRemoteServer skips config normalization, so a loopback address
	// stands in for the Tailscale address here.
	require.NoError(t, server.startRemoteServer(config.RemoteAPIConfig{Enabled: true, Listen: addr}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.remoteServer.Shutdown(ctx)
	})

	resp, err := http.Get(fmt.Sprintf("http://%s/api/ping", addr))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, 5*time.Second, server.remoteServer.ReadHeaderTimeout)
	assert.Zero(t, server.remoteServer.ReadTimeout)
	assert.Zero(t, server.remoteServer.WriteTimeout)
}

func TestStartRemoteServerDisabled(t *testing.T) {
	server, _, _ := newTestServer(t)
	require.NoError(t, server.startRemoteServer(config.RemoteAPIConfig{}))
	assert.Nil(t, server.remoteServer)
}
