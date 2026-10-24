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

func loadKataIntegration(t *testing.T, contents string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return LoadGlobalFrom(path)
}

func TestKataIntegrationAccepted(t *testing.T) {
	tests := []struct {
		name, contents string
		want           KataIntegrationConfig
		wantKind       KataEndpointKind
	}{
		{
			name: "https with inline key",
			contents: "[integrations.kata]\nenabled = true\nendpoint = \"https://kata.example.com\"\n" +
				"api_key = \"k\"\ndefault_project = \"reviews\"\nteammate = \"review-bot\"\n",
			want: KataIntegrationConfig{
				Enabled: true, Endpoint: "https://kata.example.com", APIKey: "k",
				DefaultProject: "reviews", Teammate: "review-bot",
			},
			wantKind: KataEndpointHTTPS,
		},
		{
			name: "loopback http with key file and default teammate",
			contents: "[integrations.kata]\nenabled = true\nendpoint = \"http://127.0.0.1:7777\"\n" +
				"api_key_file = \"/etc/roborev/kata-token\"\n",
			want: KataIntegrationConfig{
				Enabled: true, Endpoint: "http://127.0.0.1:7777",
				APIKeyFile: "/etc/roborev/kata-token", Teammate: DefaultKataTeammate,
			},
			wantKind: KataEndpointLoopbackHTTP,
		},
		{
			name:     "unix socket needs no key",
			contents: "[integrations.kata]\nenabled = true\nendpoint = \"unix:///run/kata/api.sock\"\n",
			want: KataIntegrationConfig{
				Enabled: true, Endpoint: "unix:///run/kata/api.sock", Teammate: DefaultKataTeammate,
			},
			wantKind: KataEndpointUnix,
		},
		{
			name:     "disabled settings are not validated",
			contents: "[integrations.kata]\nenabled = false\nendpoint = \"ftp://nope\"\nteammate = \"-bad\"\n",
			want: KataIntegrationConfig{
				Endpoint: "ftp://nope", Teammate: "-bad",
			},
		},
		{
			name: "absent section keeps the integration off",
			want: KataIntegrationConfig{Teammate: DefaultKataTeammate},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadKataIntegration(t, tt.contents)
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.Integrations.Kata)
			if tt.want.Enabled {
				endpoint, err := cfg.Integrations.Kata.ParsedEndpoint()
				require.NoError(t, err)
				assert.Equal(t, tt.wantKind, endpoint.Kind)
			}
		})
	}
}

func TestKataIntegrationRejected(t *testing.T) {
	enabled := "[integrations.kata]\nenabled = true\n"
	tests := []struct{ name, contents, wantErr string }{
		{"missing endpoint", enabled, "endpoint"},
		{"unsupported scheme", enabled + "endpoint = \"ftp://kata.example.com\"\n", "unsupported scheme"},
		{"remote plaintext http", enabled + "endpoint = \"http://kata.example.com\"\napi_key = \"k\"\n", "plaintext"},
		{"userinfo", enabled + "endpoint = \"https://user:pw@kata.example.com\"\n", "userinfo"},
		{"query", enabled + "endpoint = \"https://kata.example.com/?x=1\"\n", "query"},
		{"fragment", enabled + "endpoint = \"https://kata.example.com/#x\"\n", "fragment"},
		{"https without host", enabled + "endpoint = \"https://\"\n", "host"},
		{"unix with host", enabled + "endpoint = \"unix://host/run/api.sock\"\n", "unix"},
		{"unix with relative path", enabled + "endpoint = \"unix:run/api.sock\"\n", "unix"},
		{"loopback http without key", enabled + "endpoint = \"http://localhost:7777\"\n", "api_key"},
		{"both key sources", enabled + "endpoint = \"https://kata.example.com\"\napi_key = \"k\"\napi_key_file = \"/f\"\n", "mutually exclusive"},
		{"teammate with leading dash", enabled + "endpoint = \"https://kata.example.com\"\nteammate = \"-bot\"\n", "teammate"},
		{"teammate with space", enabled + "endpoint = \"https://kata.example.com\"\nteammate = \"review bot\"\n", "teammate"},
		{"teammate too long", enabled + "endpoint = \"https://kata.example.com\"\nteammate = \"" + strings.Repeat("a", 65) + "\"\n", "teammate"},
		{"default project with newline", enabled + "endpoint = \"https://kata.example.com\"\ndefault_project = \"a\\nb\"\n", "default_project"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadKataIntegration(t, tt.contents)
			require.ErrorContains(t, err, tt.wantErr)
			assert.ErrorContains(t, err, "integrations.kata")
		})
	}
}

func TestKataIntegrationErrorsNeverEchoTheKey(t *testing.T) {
	const secret = "synthetic-kata-secret-9f3a"
	_, err := loadKataIntegration(t, "[integrations.kata]\nenabled = true\napi_key = \""+secret+"\"\n"+
		"api_key_file = \"/f\"\nendpoint = \"https://kata.example.com\"\n")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
}

func TestKataIntegrationIsGlobalOnly(t *testing.T) {
	for _, key := range []string{
		"integrations.kata.enabled", "integrations.kata.endpoint", "integrations.kata.api_key",
		"integrations.kata.api_key_file", "integrations.kata.default_project", "integrations.kata.teammate",
	} {
		assert.True(t, IsGlobalKey(key), key)
		assert.True(t, IsValidKey(key), key)
	}
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".roborev.toml"),
		[]byte("[integrations.kata]\nenabled = true\nendpoint = \"https://kata.example.com\"\n"), 0o600))
	_, err := LoadRepoConfig(root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "global-only")
	assert.Contains(t, err.Error(), "integrations.kata")
}

func TestKataIntegrationKeyIsMaskedAndNeverSerialized(t *testing.T) {
	assert.True(t, IsSensitiveKey("integrations.kata.api_key"))
	assert.False(t, IsSensitiveKey("integrations.kata.api_key_file"), "a path is not a secret")

	cfg := DefaultConfig()
	cfg.Integrations.Kata.APIKey = "synthetic-kata-secret-9f3a"
	encoded, err := json.Marshal(cfg)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "synthetic-kata-secret-9f3a")
}

func TestKataIntegrationResolveAPIKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name, contents string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
		return path
	}
	t.Run("inline key", func(t *testing.T) {
		key, err := KataIntegrationConfig{APIKey: " k1 "}.ResolveAPIKey()
		require.NoError(t, err)
		assert.Equal(t, "k1", key)
	})
	t.Run("no key", func(t *testing.T) {
		key, err := KataIntegrationConfig{}.ResolveAPIKey()
		require.NoError(t, err)
		assert.Empty(t, key)
	})
	t.Run("file with trailing newline", func(t *testing.T) {
		key, err := KataIntegrationConfig{APIKeyFile: write("nl", "file-key\n")}.ResolveAPIKey()
		require.NoError(t, err)
		assert.Equal(t, "file-key", key)
	})
	t.Run("home relative file", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		require.NoError(t, os.WriteFile(filepath.Join(home, "kata-token"), []byte("home-key"), 0o600))
		key, err := KataIntegrationConfig{APIKeyFile: "~/kata-token"}.ResolveAPIKey()
		require.NoError(t, err)
		assert.Equal(t, "home-key", key)
	})
	t.Run("empty file", func(t *testing.T) {
		_, err := KataIntegrationConfig{APIKeyFile: write("empty", "\n")}.ResolveAPIKey()
		require.ErrorContains(t, err, "empty")
	})
	t.Run("file with inner whitespace", func(t *testing.T) {
		_, err := KataIntegrationConfig{APIKeyFile: write("ws", "two words")}.ResolveAPIKey()
		require.ErrorContains(t, err, "single")
	})
	t.Run("large file", func(t *testing.T) {
		want := strings.Repeat("k", (1<<20)+1)
		key, err := KataIntegrationConfig{APIKeyFile: write("big", want)}.ResolveAPIKey()
		require.NoError(t, err)
		assert.Equal(t, want, key)
	})
	t.Run("both sources", func(t *testing.T) {
		_, err := KataIntegrationConfig{APIKey: "k", APIKeyFile: write("both", "k")}.ResolveAPIKey()
		require.ErrorContains(t, err, "mutually exclusive")
	})
}

func TestValidateKataTeammatePolicyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "single letter", input: "a"},
		{name: "allowed punctuation", input: "a.b_c-d"},
		{name: "maximum length", input: strings.Repeat("a", 64)},
		{name: "empty", input: "", wantErr: true},
		{name: "leading dash", input: "-a", wantErr: true},
		{name: "space", input: "a b", wantErr: true},
		{name: "over maximum length", input: strings.Repeat("a", 65), wantErr: true},
		{name: "non ASCII", input: "é", wantErr: true},
		{name: "newline", input: "a\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateKataTeammate(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestParseKataEndpointForms(t *testing.T) {
	tests := []struct {
		raw        string
		wantKind   KataEndpointKind
		wantSocket string
		wantBase   string
	}{
		{"https://kata.example.com", KataEndpointHTTPS, "", "https://kata.example.com"},
		{"https://kata.example.com/proxy/", KataEndpointHTTPS, "", "https://kata.example.com/proxy/"},
		{"http://localhost:7777", KataEndpointLoopbackHTTP, "", "http://localhost:7777"},
		{"http://127.0.0.1:7777", KataEndpointLoopbackHTTP, "", "http://127.0.0.1:7777"},
		{"http://[::1]:7777", KataEndpointLoopbackHTTP, "", "http://[::1]:7777"},
		{"unix:///run/kata.sock", KataEndpointUnix, "/run/kata.sock", "http://unix"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			endpoint, err := ParseKataEndpoint(tt.raw)
			require.NoError(t, err)
			assert.Equal(t, tt.wantKind, endpoint.Kind)
			assert.Equal(t, tt.wantSocket, endpoint.SocketPath)
			assert.Equal(t, tt.wantBase, endpoint.URL.String())
		})
	}
}

func TestParseKataEndpointRejectionsAreInsecureEndpointErrors(t *testing.T) {
	for _, raw := range []string{
		"", "kata.example.com", "ftp://x", "http://kata.example.com", "https://u@x", "https://x?q=1",
		"https://x#f", "https://", "https://:443", "unix://h/p", "unix:p",
	} {
		_, err := ParseKataEndpoint(raw)
		require.Error(t, err, raw)
		assert.ErrorIs(t, err, ErrKataEndpoint, raw)
	}
}

func FuzzParseKataEndpoint(f *testing.F) {
	for _, seed := range []string{
		"https://kata.example.com", "http://localhost", "unix:///a.sock", "http://evil.example.com@127.0.0.1",
		"https://user@host", "HTTPS://HOST", "http://127.0.0.1.evil.com", "unix:///a%00b", "https://[::1", "https://a b",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		endpoint, err := ParseKataEndpoint(raw)
		if err != nil {
			require.ErrorIs(t, err, ErrKataEndpoint, raw)
			return
		}
		require.NotNil(t, endpoint.URL)
		require.Nil(t, endpoint.URL.User, "%q", raw)
		require.Empty(t, endpoint.URL.RawQuery, "%q", raw)
		require.Empty(t, endpoint.URL.Fragment, "%q", raw)
		switch endpoint.Kind {
		case KataEndpointHTTPS:
			require.Equal(t, "https", endpoint.URL.Scheme)
			require.NotEmpty(t, endpoint.URL.Hostname())
		case KataEndpointLoopbackHTTP:
			require.Equal(t, "http", endpoint.URL.Scheme)
			require.True(t, isLoopbackHost(endpoint.URL.Hostname()), "%q", raw)
		case KataEndpointUnix:
			require.True(t, strings.HasPrefix(endpoint.SocketPath, "/"), "%q", raw)
			require.Equal(t, "http://unix", endpoint.URL.String())
		default:
			require.Failf(t, "unknown kind", "%q -> %q", raw, endpoint.Kind)
		}
	})
}
