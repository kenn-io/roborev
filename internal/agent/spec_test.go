package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/config"
)

func TestAgentSpecsResolveAliasesAndCanonicalNames(t *testing.T) {
	t.Parallel()

	for _, spec := range allAgentSpecs {
		assert.Equal(t, spec.Name, resolveAlias(spec.Name), "canonical name should resolve to itself: %s", spec.Name)
		for _, alias := range spec.Aliases {
			assert.Equal(t, spec.Name, resolveAlias(alias), "alias %s should resolve to %s", alias, spec.Name)
		}
	}
}

func TestAgentSpecsFallbackOrder(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"codex", "claude-code", "gemini", "copilot", "opencode", "cursor", "kiro", "kilo", "droid", "pi"}, fallbackAgentOrder)
	assert.Equal(t, fallbackAgentOrder, installHintAgentNames())
}

func TestBuildFallbackAgentOrderValidatesMetadata(t *testing.T) {
	t.Parallel()

	require.PanicsWithValue(t, "invalid fallback rank for claude-code: got 3, want 2", func() {
		buildFallbackAgentOrder([]agentSpec{
			{Name: "codex", FallbackRank: 1},
			{Name: "claude-code", FallbackRank: 3},
		})
	})

	require.PanicsWithValue(t, "duplicate fallback rank 1 for claude-code and codex", func() {
		buildFallbackAgentOrder([]agentSpec{
			{Name: "codex", FallbackRank: 1},
			{Name: "claude-code", FallbackRank: 1},
		})
	})
}

func TestAgentSpecsCommandOverrides(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		CodexCmd:      "custom-codex",
		ClaudeCodeCmd: "custom-claude",
		CursorCmd:     "custom-cursor",
		PiCmd:         "custom-pi",
		OpenCodeCmd:   "custom-opencode",
	}

	for _, spec := range allAgentSpecs {
		t.Run(spec.Name, func(t *testing.T) {
			agent, err := Get(spec.Name)
			require.NoError(t, err)

			overridden := applyCommandOverrides(agent, cfg)
			commandAgent, ok := overridden.(CommandAgent)
			if spec.DefaultCommand == "" {
				assert.False(t, ok)
				return
			}

			require.True(t, ok)
			expectedCommand := spec.DefaultCommand
			if override := commandOverrideForAgent(spec.Name, cfg); override != "" {
				expectedCommand = override
			}
			assert.Equal(t, expectedCommand, commandAgent.CommandName())
		})
	}
}

func TestApplyAgentConfigOverridesPiJSONSchemaExtension(t *testing.T) {
	t.Parallel()

	agent := NewPiAgent("pi")
	overridden := applyAgentConfigOverrides(agent, &config.Config{
		Agent: config.AgentConfig{
			Pi: config.PiConfig{
				JSONSchemaExtension: "/opt/roborev/pi-json-schema/index.ts",
			},
		},
	})

	pi, ok := overridden.(*PiAgent)
	require.True(t, ok)
	assert.Equal(t, "/opt/roborev/pi-json-schema/index.ts", pi.JSONSchemaExtension)
	assert.Equal(t, config.DefaultPiJSONSchemaExtension, agent.JSONSchemaExtension)
}
