//go:build !windows

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListenAuxiliaryEndpointForTCP(t *testing.T) {
	setShortRuntimeDir(t)

	listener, endpoint, err := listenAuxiliaryEndpoint(DaemonEndpoint{
		Network: "tcp",
		Address: "127.0.0.1:7373",
	})
	require.NoError(t, err)
	require.NotNil(t, listener)
	require.NotNil(t, endpoint)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })

	assert.Equal(t, "unix", endpoint.Network)
	info, err := os.Stat(endpoint.Address)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	dirInfo, err := os.Stat(filepath.Dir(endpoint.Address))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
}

func TestListenAuxiliaryEndpointSkipsUnixPrimary(t *testing.T) {
	setShortRuntimeDir(t)

	listener, endpoint, err := listenAuxiliaryEndpoint(DaemonEndpoint{
		Network: "unix",
		Address: DefaultSocketPath(),
	})

	require.NoError(t, err)
	assert.Nil(t, listener)
	assert.Nil(t, endpoint)
}

func TestListenAuxiliaryEndpointIsolatesDataDirectories(t *testing.T) {
	setShortRuntimeDir(t)
	primary := DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}

	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	firstListener, firstEndpoint, err := listenAuxiliaryEndpoint(primary)
	require.NoError(t, err)
	require.NotNil(t, firstListener)
	require.NotNil(t, firstEndpoint)
	t.Cleanup(func() { require.NoError(t, firstListener.Close()) })

	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	secondListener, secondEndpoint, err := listenAuxiliaryEndpoint(primary)
	require.NoError(t, err)
	require.NotNil(t, secondListener)
	require.NotNil(t, secondEndpoint)
	t.Cleanup(func() { require.NoError(t, secondListener.Close()) })

	assert.NotEqual(t, firstEndpoint.Address, secondEndpoint.Address)
}
