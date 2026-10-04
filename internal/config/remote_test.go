package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteConfigFailsClosed(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Remote.Enabled = true
	require.Error(t, validateRemoteConfig(cfg))
	cfg.AuthKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cfg.Remote = RemoteConfig{Enabled: true, ExternalURL: "https://reviews.example.com/history", TrustedProxy: true, ReplayFile: "replay.json", MaxBodyBytes: 1024, MaxHeaderBytes: 8192, MaxConcurrent: 2, ReplayCapacity: 100, ReadHeaderTimeout: "5s", ReadTimeout: "30s", RequestTimeout: "1m", Keys: []RemoteSigningKey{{ID: "reader", SecretFile: "reader.key", Grants: []string{"history:read"}, RepoIDs: []int64{1}}}}
	if runtime.GOOS == "windows" {
		require.Error(t, validateRemoteConfig(cfg))
		return
	}
	require.NoError(t, validateRemoteConfig(cfg))
	cfg.Remote.Keys[0].Grants = []string{"execute"}
	require.Error(t, validateRemoteConfig(cfg))
	cfg.Remote.Keys[0].Grants = []string{"history:read"}
	cfg.Remote.Keys[0].RepoIDs = nil
	require.Error(t, validateRemoteConfig(cfg))
	cfg.Remote.Keys[0].AllRepos = true
	require.NoError(t, validateRemoteConfig(cfg))
}

func TestLoadRemoteClientConfig(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	path := GlobalConfigPath()
	require.NoError(t, os.WriteFile(path, []byte("[remote_client]\nkey_id='reader'\nsecret_file='reader.key'\nca_file='ca.pem'\n"), 0o600))
	cfg, err := LoadRemoteClient()
	require.NoError(t, err)
	assert.Equal(t, "reader", cfg.KeyID)
	assert.Equal(t, "reader.key", cfg.SecretFile)
	assert.Equal(t, "ca.pem", cfg.CAFile)
	require.NoError(t, os.WriteFile(filepath.Clean(path), []byte("[remote_client]\nsecret_file = secret-never-display"), 0o600))
	_, err = LoadRemoteClient()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret-never-display")
}
