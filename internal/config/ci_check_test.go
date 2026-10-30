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
			assert.Equal(t, override, ResolveCICheck(&RepoConfig{CI: RepoCIConfig{Check: &override}}, cfg))
		}
	}
	cfg := DefaultConfig()
	cfg.CI.CheckReasoning = "invalid"
	require.ErrorContains(t, cfg.Validate(), "ci.check_reasoning")
	cfg.CI.CheckReasoning = "high"
	require.NoError(t, cfg.Validate())
}
