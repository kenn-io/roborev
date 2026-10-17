package config

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Client settings must survive a daemon section that cannot run on this host.
func TestLoadGlobalPreservesClientSettingsWithInvalidDaemonConfig(t *testing.T) {
	for _, section := range []string{
		"[web]\nauth_token_file = 'missing-token'\n",
		"[web]\nlisten = '0.0.0.0:7374'\n",
		"[search.embeddings]\nbase_url = 'https://example.test/v1'\n",
		"[remote]\nenabled = true\n",
		"[budget]\nenabled = true\n",
		"[ci.github_app_installations]\nExample = 1\nexample = 2\n",
	} {
		t.Run(section, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ROBOREV_DATA_DIR", dir)
			path := filepath.Join(dir, "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("hook_timeout_seconds = 17\n"+section), 0o600))
			cfg, err := LoadGlobal()
			require.NoError(t, err)
			assert.Equal(t, 17, cfg.HookTimeoutSeconds)
			_, err = LoadGlobalFrom(path)
			require.Error(t, err, "daemon loading must remain strict")
		})
	}
}

func FuzzLoadGlobalUnrelatedWebListen(f *testing.F) {
	for _, listen := range []string{"0.0.0.0:7374", "invalid", "127.0.0.1:0", "[::1]:0"} {
		f.Add(listen, 17)
	}
	f.Fuzz(func(t *testing.T, listen string, timeout int) {
		if !utf8.ValidString(listen) {
			t.Skip("TOML strings require UTF-8")
		}
		// Quoting through TOML's encoder keeps this a semantic-validation property.
		data, ok := encodeTOMLOverrideValue(listen)
		require.True(t, ok)
		dir := t.TempDir()
		t.Setenv("ROBOREV_DATA_DIR", dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("hook_timeout_seconds = %d\n[web]\nlisten = %s\n", timeout, data)), 0o600))
		loaded, loadErr := LoadGlobal()
		require.NoError(t, loadErr)
		assert.Equal(t, timeout, loaded.HookTimeoutSeconds)
		assert.Equal(t, listen, loaded.Web.Listen)
	})
}

func TestDaemonDiagnosticsDoNotNormalizeClientValues(t *testing.T) {
	assert := assert.New(t)
	cfg := DefaultConfig()
	cfg.AuthKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cfg.Web.PublicOrigin = "HTTPS://REVIEWS.EXAMPLE.COM:443"
	cfg.CI.GitHubAppInstallations = map[string]int64{"Example": 1}
	require.Empty(t, cfg.DaemonConfigWarnings())
	assert.Equal("HTTPS://REVIEWS.EXAMPLE.COM:443", cfg.Web.PublicOrigin)
	assert.Equal(map[string]int64{"Example": 1}, cfg.CI.GitHubAppInstallations)
	require.NoError(t, cfg.ValidateDaemon())
	assert.Equal("https://reviews.example.com", cfg.Web.PublicOrigin)
	assert.Equal(map[string]int64{"example": 1}, cfg.CI.GitHubAppInstallations)
}

func FuzzDaemonWarningsPreserveInstallations(f *testing.F) {
	f.Add("Example", false)
	f.Add("Another", true)
	f.Fuzz(func(t *testing.T, owner string, collide bool) {
		cfg := DefaultConfig()
		cfg.CI.GitHubAppInstallations = map[string]int64{owner: 1, "Example": 2}
		if collide {
			cfg.CI.GitHubAppInstallations["EXAMPLE"] = 3
		}
		before := maps.Clone(cfg.CI.GitHubAppInstallations)
		for range 2 {
			cfg.DaemonConfigWarnings()
			assert.Equal(t, before, cfg.CI.GitHubAppInstallations)
		}
	})
}
