package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadGlobalPreservesClientSettingsWithInvalidDaemonConfig(t *testing.T) {
	for _, tc := range []struct{ section, contents string }{
		{"web", "[web]\nauth_token_file = 'missing-token'\n"},
		{"web", "[web]\nauth_token_file = 'missing-token'\n[ci.github_app_installations]\nExample = 1\n"},
		{"web", "[web]\nlisten = '0.0.0.0:7374'\n"},
		{"search", "[search.embeddings]\nbase_url = 'https://example.test/v1'\n"},
		{"remote", "[remote]\nenabled = true\n"},
		{"budget", "[budget]\nenabled = true\n"},
		{"ci", "[ci.github_app_installations]\nExample = 1\nexample = 2\n"},
	} {
		t.Run(tc.contents, func(t *testing.T) {
			assert := assert.New(t)
			dir := t.TempDir()
			t.Setenv("ROBOREV_DATA_DIR", dir)
			path := filepath.Join(dir, "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("hook_timeout_seconds = 17\n"+tc.contents), 0o600))
			cfg, err := LoadGlobal()
			require.NoError(t, err)
			assert.Equal(17, cfg.HookTimeoutSeconds)
			_, err = LoadGlobalFrom(path)
			require.ErrorContains(t, err, "["+tc.section+"]")

			warnings := cfg.DaemonConfigWarnings()
			require.Len(t, warnings, 1)
			assert.Contains(warnings[0].Error(), "["+tc.section+"]")
			if tc.section == "web" && len(cfg.CI.GitHubAppInstallations) > 0 {
				assert.Equal(map[string]int64{"example": 1}, cfg.CI.GitHubAppInstallations)
			}
		})
	}
}
