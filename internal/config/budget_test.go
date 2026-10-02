package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBudgetConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[budget]
enabled = true
daily_limit_cents = 500
reserve_floor_cents = 100
[budget.agent_costs]
codex = 12
"acp.reviewer" = 5
`), 0o600))
	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	got, err := GetConfigValue(cfg, "budget.daily_limit_cents")
	require.NoError(t, err)
	assert.Equal(t, "500", got)
	require.NoError(t, SetConfigValue(cfg, "budget.agent_costs.codex", "15"))
	got, err = GetConfigValue(cfg, "budget.agent_costs.codex")
	require.NoError(t, err)
	assert.Equal(t, "15", got)
	require.NoError(t, SetConfigValue(cfg, "budget.agent_costs.acp.reviewer", "6"))
	got, err = GetConfigValue(cfg, "budget.agent_costs.acp.reviewer")
	require.NoError(t, err)
	assert.Equal(t, "6", got)
	assert.True(t, IsGlobalKey("budget.agent_costs.gemini"))
	_, err = GetConfigValue(&RepoConfig{}, "budget.enabled")
	require.Error(t, err)
	require.NoError(t, SaveGlobalTo(path, cfg))
	cfg, err = LoadGlobalFrom(path)
	require.NoError(t, err)
	got, err = GetConfigValue(cfg, "budget.agent_costs.acp.reviewer")
	require.NoError(t, err)
	assert.Equal(t, "6", got)
}

func TestBudgetValidation(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing limit", "enabled = true"},
		{"negative limit", "daily_limit_cents = -1"},
		{"negative reserve", "reserve_floor_cents = -1"},
		{"reserve above limit", "daily_limit_cents = 5\nreserve_floor_cents = 6"},
		{"zero price", "[budget.agent_costs]\ncodex = 0"},
		{"negative price", "[budget.agent_costs]\ncodex = -2"},
		{"unknown agent", "[budget.agent_costs]\nunknown = 5"},
		{"test agent", "[budget.agent_costs]\ntest = 1"},
		{"duplicate alias", "[budget.agent_costs]\nclaude = 5\nclaude-code = 12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("[budget]\n"+tc.body+"\n"), 0o600))
			_, err := LoadGlobalFrom(path)
			require.ErrorContains(t, err, "budget")
		})
	}
}

func TestBudgetPriceListingAndRemoval(t *testing.T) {
	cfg := DefaultConfig()
	require.NoError(t, SetConfigValue(cfg, "budget.agent_costs.acp.reviewer", "6"))
	assert.Contains(t, ListConfigKeys(cfg), KeyValue{Key: "budget.agent_costs.acp.reviewer", Value: "6"})
	raw := map[string]any{"budget": map[string]any{"agent_costs": map[string]any{"acp.reviewer": int64(6)}}}
	assert.True(t, IsKeyInTOMLFile(raw, "budget.agent_costs.acp.reviewer"))
	require.NoError(t, SetConfigValue(cfg, "budget.agent_costs.acp.reviewer", ""))
	assert.Empty(t, cfg.Budget.AgentCosts)
}
