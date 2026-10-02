package config

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthKeyConfig(t *testing.T) {
	a := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`auth_key = "test-shared-key"`), 0o600))
	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	value, err := GetConfigValue(cfg, "auth_key")
	require.NoError(t, err)
	a.Equal("test-shared-key", value)
	a.True(IsSensitiveKey("auth_key"))
	a.True(IsGlobalKey("auth_key"))
	_, err = GetConfigValue(&RepoConfig{}, "auth_key")
	require.Error(t, err)
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	a.NotContains(string(data), "test-shared-key")
	require.NoError(t, SaveGlobalTo(path, cfg))
	saved, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	a.Equal("test-shared-key", saved.AuthKey)
}

func TestAuthKeyInvalidConfig(t *testing.T) {
	for _, value := range []string{`"bad key"`, `"bad\nkey"`, `"bad:key"`, `"nonascii-é"`, `"=bad"`, `"bad=key"`} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("auth_key = "+value), 0o600))
			_, err := LoadGlobalFrom(path)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), value)
		})
	}
}

func TestAuthKeyMalformedConfigDoesNotExposeSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("auth_key = secret-never-print"), 0o600))
	_, err := LoadGlobalFrom(path)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret-never-print")
	assert.Contains(t, err.Error(), `last key "auth_key"`)
}
