package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKataFindingHookValidation(t *testing.T) {
	for _, mode := range []string{"comments", "summary"} {
		cfg := DefaultConfig()
		cfg.Hooks = []HookConfig{{Type: "kata", Findings: mode}}
		require.ErrorContains(t, cfg.Validate(), "Kata API connection")
		cfg.Integrations.Kata = KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example"}
		require.NoError(t, cfg.Validate())
	}
	cfg := DefaultConfig()
	cfg.Hooks = []HookConfig{{Type: "kata", Findings: "typo"}}
	require.ErrorContains(t, cfg.Validate(), "findings")
	cfg.Hooks = []HookConfig{{Type: "command", Findings: "summary"}}
	require.ErrorContains(t, cfg.Validate(), "Kata hook")
}
