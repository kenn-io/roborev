package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCICheckConfig(t *testing.T) {
	for _, global := range []bool{false, true} {
		cfg := DefaultConfig()
		cfg.CI.Check = global
		assert.Equal(t, global, ResolveCICheck(nil, cfg))
		for _, override := range []bool{false, true} {
			assert.Equal(t, override, ResolveCICheck(&RepoConfig{CI: RepoCIConfig{Check: &override}}, cfg))
		}
	}
	cfg := DefaultConfig()
	cfg.CI.CheckReasoning = "invalid"
	require.ErrorContains(t, cfg.Validate(), "ci.check_reasoning")
	cfg.CI.CheckReasoning = "high"
	require.NoError(t, cfg.Validate())
	for _, key := range []string{"check_model", "check_reasoning"} {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte("[ci]\n"+key+" = \"codex\"\n"), 0o600))
		_, err := LoadRepoConfig(dir)
		require.ErrorContains(t, err, "ci."+key+`" is global-only`)
	}
}
