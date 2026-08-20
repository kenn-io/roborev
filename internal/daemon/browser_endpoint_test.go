package daemon

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestBrowserEndpointResolution(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		endpoint, err := ResolveBrowserEndpoint(config.WebConfig{Enabled: false})
		require.NoError(t, err)
		assert.False(t, endpoint.Enabled)
		assert.Nil(t, endpoint.Listener)
	})

	t.Run("ephemeral loopback", func(t *testing.T) {
		endpoint, err := ResolveBrowserEndpoint(config.WebConfig{Enabled: true, Listen: "127.0.0.1:0"})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, endpoint.Listener.Close()) })
		assert.True(t, endpoint.Enabled)
		assert.Equal(t, endpoint.Listener.Addr().String(), endpoint.Address)
		assert.Equal(t, "http://"+endpoint.Address, endpoint.Origin)
		assert.NotEmpty(t, endpoint.DialAddress)
	})

	t.Run("public origin does not change bind", func(t *testing.T) {
		endpoint, err := ResolveBrowserEndpoint(config.WebConfig{
			Enabled: true, Listen: "127.0.0.1:0", PublicOrigin: "https://reviews.example.com", AuthToken: testBrowserAuthToken,
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, endpoint.Listener.Close()) })
		assert.Equal(t, "https://reviews.example.com", endpoint.Origin)
		assert.Contains(t, endpoint.Address, "127.0.0.1:")
		assert.Equal(t, "token", endpoint.authentication)
	})

	t.Run("proxy authentication", func(t *testing.T) {
		endpoint, err := ResolveBrowserEndpoint(config.WebConfig{
			Enabled:      true,
			Listen:       "127.0.0.1:0",
			PublicOrigin: "https://reviews.example.com",
			AuthMode:     config.WebAuthModeProxy,
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, endpoint.Listener.Close()) })
		assert.Equal(t, "proxy", endpoint.authentication)
	})

	t.Run("rejects a non-loopback bind without normalized config", func(t *testing.T) {
		endpoint, err := ResolveBrowserEndpoint(config.WebConfig{
			Enabled: true, Listen: "0.0.0.0:0", PublicOrigin: "https://reviews.example.com", AuthToken: testBrowserAuthToken,
		})
		if endpoint.Listener != nil {
			t.Cleanup(func() { require.NoError(t, endpoint.Listener.Close()) })
		}
		require.ErrorContains(t, err, "loopback")
	})
}

func TestBrowserDialAddressUsesLoopbackForUnspecifiedBind(t *testing.T) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	assert.True(t, net.ParseIP(dialAddress(listener.Addr()).Hostname()).IsLoopback())
}
