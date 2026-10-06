package config

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"regexp"
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

func TestDaemonTLSConfigRejectsPartialSettings(t *testing.T) {
	// '{pki}name' becomes a TOML string holding an absolute path for the host
	// OS. The directory name has an apostrophe so quoting is exercised.
	pkiPath := regexp.MustCompile(`'\{pki\}([^']*)'`)
	for _, tc := range []struct {
		name    string
		section string
		wantErr string
	}{
		{name: "client only", section: `ca_file = '{pki}ca.pem'
client_cert_file = '{pki}client.pem'
client_key_file = '{pki}client-key.pem'`},
		{name: "certificate without CA", section: `client_cert_file = '{pki}client.pem'`, wantErr: "ca_file is required"},
		{name: "CA without client certificate", section: `ca_file = '{pki}ca.pem'`, wantErr: "client_cert_file and client_key_file are required"},
		{name: "server certificate without key", section: `ca_file = '{pki}ca.pem'
cert_file = '{pki}daemon.pem'
client_cert_file = '{pki}client.pem'
client_key_file = '{pki}client-key.pem'`, wantErr: "cert_file and key_file must be set together"},
		{name: "relative path", section: `ca_file = 'ca.pem'`, wantErr: "ca_file must be an absolute path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			pki := filepath.Join(t.TempDir(), "user's pki")
			section := pkiPath.ReplaceAllStringFunc(tc.section, func(match string) string {
				encoded, ok := encodeTOMLOverrideValue(filepath.Join(pki, pkiPath.FindStringSubmatch(match)[1]))
				require.True(t, ok)
				return encoded
			})
			contents := "[daemon_tls]\n" + section + "\n"
			require.NoError(t, os.WriteFile(GlobalConfigPath(), []byte(contents), 0o600))
			_, err := LoadGlobalClientAuth()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
