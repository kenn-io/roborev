package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadGlobalPreservesClientSettingsWithInvalidDaemonConfig(t *testing.T) {
	for _, tc := range []struct {
		section, contents string
		installations     map[string]int64
	}{
		{"web", "[web]\nauth_token_file = 'missing-token'\n[ci.github_app_installations]\nExample = 1\n", map[string]int64{"example": 1}},
		{"search", "[search.embeddings]\nbase_url = 'https://example.test/v1'\n", nil},
		{"remote", "[remote]\nenabled = true\n", nil},
		{"budget", "[budget]\nenabled = true\n", nil},
		{"ci", "[ci.github_app_installations]\nExample = 1\nexample = 2\n", nil},
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
			if tc.installations != nil {
				assert.Equal(tc.installations, cfg.CI.GitHubAppInstallations)
			}
		})
	}
}
