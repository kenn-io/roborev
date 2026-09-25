package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteAPIConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "disabled ignores listen", body: "[remote_api]\nlisten = \"0.0.0.0:1\"\n"},
		{name: "tailscale ipv4", body: "[remote_api]\nenabled = true\nlisten = \"100.101.102.103:7474\"\n"},
		{name: "tailscale ipv6", body: "[remote_api]\nenabled = true\nlisten = \"[fd7a:115c:a1e0::1]:7474\"\n"},
		{name: "lan address", body: "[remote_api]\nenabled = true\nlisten = \"192.168.1.5:7474\"\n", wantErr: "not a Tailscale address"},
		{name: "unspecified", body: "[remote_api]\nenabled = true\nlisten = \"0.0.0.0:7474\"\n", wantErr: "not a Tailscale address"},
		{name: "hostname", body: "[remote_api]\nenabled = true\nlisten = \"host.example:7474\"\n", wantErr: "must be a Tailscale IP and port"},
		{name: "zero port", body: "[remote_api]\nenabled = true\nlisten = \"100.101.102.103:0\"\n", wantErr: "fixed port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadGlobalFrom(writeGlobalConfig(t, tt.body))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, cfg)
		})
	}
}

func TestRemoteClientConfigParses(t *testing.T) {
	cfg, err := LoadGlobalFrom(writeGlobalConfig(t,
		"[remote]\nserver = \"http://daemon-host.example:7474\"\n"))
	require.NoError(t, err)
	assert.Equal(t, "http://daemon-host.example:7474", cfg.Remote.Server)
}

func TestLoadRemoteServerIgnoresUnrelatedInvalidSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	server, err := LoadRemoteServer()
	require.NoError(t, err, "missing config file means no remote server")
	assert.Empty(t, server)

	// remote_api.listen is invalid, which full validation rejects, but
	// reading [remote] server must still work.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(
		"[remote_api]\nenabled = true\nlisten = \"0.0.0.0:1\"\n[remote]\nserver = \"http://daemon-host.example:7474\"\n"), 0o644))
	server, err = LoadRemoteServer()
	require.NoError(t, err)
	assert.Equal(t, "http://daemon-host.example:7474", server)
}

func TestIsTailscaleAddr(t *testing.T) {
	assert := assert.New(t)
	assert.True(IsTailscaleAddr(netip.MustParseAddr("100.64.0.1")))
	assert.True(IsTailscaleAddr(netip.MustParseAddr("100.127.255.254")))
	assert.False(IsTailscaleAddr(netip.MustParseAddr("100.128.0.1")))
	assert.True(IsTailscaleAddr(netip.MustParseAddr("fd7a:115c:a1e0::5")))
	assert.True(IsTailscaleAddr(netip.MustParseAddr("::ffff:100.64.0.1")))
	assert.False(IsTailscaleAddr(netip.MustParseAddr("10.0.0.1")))
}
