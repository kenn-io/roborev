package agent

import (
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

type agentSpec struct {
	Name             string
	DefaultCommand   string
	Aliases          []string
	FallbackRank     int
	CommandOverride  func(*config.Config) string
	CloneWithCommand func(Agent, string) Agent
}

var allAgentSpecs = []agentSpec{
	{
		Name:           "codex",
		DefaultCommand: "codex",
		FallbackRank:   1,
		CommandOverride: func(cfg *config.Config) string {
			return cfg.CodexCmd
		},
		CloneWithCommand: func(a Agent, command string) Agent {
			agent, ok := a.(*CodexAgent)
			if !ok {
				return a
			}
			clone := *agent
			clone.Command = command
			return &clone
		},
	},
	{
		Name:           "claude-code",
		DefaultCommand: "claude",
		Aliases:        []string{"claude"},
		FallbackRank:   2,
		CommandOverride: func(cfg *config.Config) string {
			return cfg.ClaudeCodeCmd
		},
		CloneWithCommand: func(a Agent, command string) Agent {
			agent, ok := a.(*ClaudeAgent)
			if !ok {
				return a
			}
			clone := *agent
			clone.Command = command
			return &clone
		},
	},
	{
		Name:           "gemini",
		DefaultCommand: "gemini",
		FallbackRank:   3,
		CloneWithCommand: func(a Agent, command string) Agent {
			agent, ok := a.(*GeminiAgent)
			if !ok {
				return a
			}
			clone := *agent
			clone.Command = command
			clone.CommandAuto = true
			return &clone
		},
	},
	{
		Name:           "copilot",
		DefaultCommand: "copilot",
		FallbackRank:   4,
	},
	{
		Name:           "opencode",
		DefaultCommand: "opencode",
		FallbackRank:   5,
		CommandOverride: func(cfg *config.Config) string {
			return cfg.OpenCodeCmd
		},
		CloneWithCommand: func(a Agent, command string) Agent {
			agent, ok := a.(*OpenCodeAgent)
			if !ok {
				return a
			}
			clone := *agent
			clone.Command = command
			return &clone
		},
	},
	{
		Name:           "cursor",
		DefaultCommand: "agent",
		Aliases:        []string{"agent"},
		FallbackRank:   6,
		CommandOverride: func(cfg *config.Config) string {
			return cfg.CursorCmd
		},
		CloneWithCommand: func(a Agent, command string) Agent {
			agent, ok := a.(*CursorAgent)
			if !ok {
				return a
			}
			clone := *agent
			clone.Command = command
			return &clone
		},
	},
	{
		Name:           "kiro",
		DefaultCommand: "kiro-cli",
		FallbackRank:   7,
	},
	{
		Name:           "kilo",
		DefaultCommand: "kilo",
		FallbackRank:   8,
	},
	{
		Name:           "droid",
		DefaultCommand: "droid",
		FallbackRank:   9,
	},
	{
		Name:           "pi",
		DefaultCommand: "pi",
		FallbackRank:   10,
		CommandOverride: func(cfg *config.Config) string {
			return cfg.PiCmd
		},
		CloneWithCommand: func(a Agent, command string) Agent {
			agent, ok := a.(*PiAgent)
			if !ok {
				return a
			}
			clone := *agent
			clone.Command = command
			return &clone
		},
	},
	{
		Name:           defaultACPName,
		DefaultCommand: defaultACPCommand,
	},
	{
		Name: "test",
	},
}

var agentSpecsByName = buildAgentSpecsByName()
var fallbackAgentOrder = buildFallbackAgentOrder(allAgentSpecs)

func buildAgentSpecsByName() map[string]agentSpec {
	specs := make(map[string]agentSpec, len(allAgentSpecs))
	for _, spec := range allAgentSpecs {
		specs[spec.Name] = spec
		for _, alias := range spec.Aliases {
			specs[alias] = spec
		}
	}
	return specs
}

func buildFallbackAgentOrder(specs []agentSpec) []string {
	fallbackSpecs := make([]agentSpec, 0, len(specs))
	for _, spec := range specs {
		if spec.FallbackRank > 0 {
			fallbackSpecs = append(fallbackSpecs, spec)
		}
	}

	slices.SortFunc(fallbackSpecs, func(a, b agentSpec) int {
		if a.FallbackRank != b.FallbackRank {
			return a.FallbackRank - b.FallbackRank
		}
		return strings.Compare(a.Name, b.Name)
	})

	for i := 1; i < len(fallbackSpecs); i++ {
		if fallbackSpecs[i-1].FallbackRank == fallbackSpecs[i].FallbackRank {
			panic(fmt.Sprintf("duplicate fallback rank %d for %s and %s", fallbackSpecs[i].FallbackRank, fallbackSpecs[i-1].Name, fallbackSpecs[i].Name))
		}
	}

	fallbacks := make([]string, 0, len(fallbackSpecs))
	for i, spec := range fallbackSpecs {
		expectedRank := i + 1
		if spec.FallbackRank != expectedRank {
			panic(fmt.Sprintf("invalid fallback rank for %s: got %d, want %d", spec.Name, spec.FallbackRank, expectedRank))
		}
		fallbacks = append(fallbacks, spec.Name)
	}

	return fallbacks
}

func resolveAlias(name string) string {
	name = strings.TrimSpace(name)
	if spec, ok := agentSpecsByName[name]; ok {
		return spec.Name
	}
	return name
}

func commandOverrideForAgent(name string, cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	spec, ok := agentSpecsByName[resolveAlias(name)]
	if !ok || spec.CommandOverride == nil {
		return ""
	}
	return strings.TrimSpace(spec.CommandOverride(cfg))
}

func applyCommandOverrides(a Agent, cfg *config.Config) Agent {
	if cfg == nil || a == nil {
		return a
	}
	spec, ok := agentSpecsByName[resolveAlias(a.Name())]
	if !ok || spec.CloneWithCommand == nil {
		return a
	}
	override := commandOverrideForAgent(spec.Name, cfg)
	if override == "" {
		return a
	}
	return spec.CloneWithCommand(a, override)
}

func installHintAgentNames() []string {
	return slices.Clone(fallbackAgentOrder)
}
