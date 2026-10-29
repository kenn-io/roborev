package config

import (
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
			repo := &RepoConfig{CI: RepoCIConfig{Check: &override}}
			assert.Equal(t, override, ResolveCICheck(repo, cfg))
		}
	}
	cfg := DefaultConfig()
	cfg.CI.CheckAgent = "claude"
	require.ErrorContains(t, cfg.Validate(), "ci.check_agent")
	cfg.CI.CheckAgent = "codex"
	cfg.CI.CheckReasoning = "invalid"
	require.ErrorContains(t, cfg.Validate(), "ci.check_reasoning")
	cfg.CI.CheckReasoning = "high"
	assert.NoError(t, cfg.Validate())
}
