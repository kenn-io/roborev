//go:build windows

package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListenUnixEndpointWindowsUnsupported(t *testing.T) {
	listener, err := listenUnixEndpoint(DaemonEndpoint{Network: "unix", Address: "/example/daemon.sock"})
	require.Nil(t, listener)
	require.Error(t, err)
	require.EqualError(t, err, "unix sockets are not supported on Windows")
}
