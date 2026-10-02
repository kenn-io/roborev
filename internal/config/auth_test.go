package config

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthKeyConfig(t *testing.T) {
	a := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`auth_key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`), 0o600))
	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	value, err := GetConfigValue(cfg, "auth_key")
	require.NoError(t, err)
	a.Equal("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", value)
	a.True(IsSensitiveKey("auth_key"))
	a.True(IsGlobalKey("auth_key"))
	_, err = GetConfigValue(&RepoConfig{}, "auth_key")
	require.Error(t, err)
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	a.NotContains(string(data), "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	require.NoError(t, SaveGlobalTo(path, cfg))
	saved, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	a.Equal("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", saved.AuthKey)
}

func TestAuthKeyInvalidConfig(t *testing.T) {
	for _, value := range []string{
		`"a"`, `"password"`, `"bad key"`, `"bad\nkey"`, `"bad:key"`, `"nonascii-é"`, `"=bad"`, `"bad=key"`,
		`"` + strings.Repeat("a", 62) + `"`,
		`"` + strings.Repeat("a", 66) + `"`,
		`"` + strings.Repeat("A", 64) + `"`,
		`"` + strings.Repeat("g", 64) + `"`,
	} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("auth_key = "+value), 0o600))
			_, err := LoadGlobalFrom(path)
			require.EqualError(t, err, "config: auth_key must contain 64 lowercase hex characters; generate a key with openssl rand -hex 32")
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
