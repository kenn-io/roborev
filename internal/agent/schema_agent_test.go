package agent

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/config"
)

type fakeSchemaAgent struct {
	*TestAgent
	name    string
	command string
	result  json.RawMessage
	err     error
}

func (f *fakeSchemaAgent) Name() string {
	if f.name != "" {
		return f.name
	}
	return f.TestAgent.Name()
}

func (f *fakeSchemaAgent) CommandName() string {
	return f.command
}

func (f *fakeSchemaAgent) WithReasoning(level ReasoningLevel) Agent {
	return f
}

func (f *fakeSchemaAgent) WithAgentic(agentic bool) Agent {
	return f
}

func (f *fakeSchemaAgent) WithModel(model string) Agent {
	return f
}

func (f *fakeSchemaAgent) ClassifyWithSchema(
	ctx context.Context, repoPath, gitRef, prompt string,
	schema json.RawMessage, out io.Writer,
) (json.RawMessage, error) {
	return f.result, f.err
}

func TestIsSchemaAgent(t *testing.T) {
	var a Agent = NewTestAgent()
	assert.False(t, IsSchemaAgent(a))

	var s Agent = &fakeSchemaAgent{TestAgent: NewTestAgent()}
	assert.True(t, IsSchemaAgent(s))
}

func TestValidateClassifyAgent_NotRegistered(t *testing.T) {
	err := ValidateClassifyAgent("no-such-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown agent")
}

func TestValidateClassifyAgent_NotSchema(t *testing.T) {
	err := ValidateClassifyAgent("test")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "structured output")
}

func TestValidateClassifyAgent_ResolvesAlias(t *testing.T) {
	// "claude" is an alias for "claude-code"; validator must canonicalize
	// before lookup so config values that the rest of the codebase
	// accepts aren't rejected here as unknown.
	require.NoError(t, ValidateClassifyAgent("claude"),
		"classify_agent = \"claude\" must be accepted via alias resolution")
}

func TestValidateClassifyAgent_CodexRejected(t *testing.T) {
	// Codex is explicitly NOT a SchemaAgent on this branch because it
	// has no way to disable shell/file tools. Regression guard so a
	// future reintroduction without equivalent hardening fails this
	// test.
	err := ValidateClassifyAgent("codex")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "structured output")
}

func TestValidateClassifyAgent_PiAccepted(t *testing.T) {
	require.NoError(t, ValidateClassifyAgent("pi"))
}

func TestGetAvailableSchemaExactWithConfigUsesCommandOverride(t *testing.T) {
	fakeBin := t.TempDir()
	wrapper := filepath.Join(fakeBin, "claude-wrapper")
	if runtime.GOOS == "windows" {
		wrapper += ".exe"
	}
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", t.TempDir())

	originalRegistry := registry
	registry = map[string]Agent{
		"claude-code": NewClaudeAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	resolved, err := GetAvailableSchemaExactWithConfig("claude", &config.Config{
		ClaudeCodeCmd: wrapper,
	})
	require.NoError(t, err)
	require.IsType(t, &ClaudeAgent{}, resolved)
	assert.Equal(t, wrapper, resolved.(*ClaudeAgent).CommandName())
}

func TestGetAvailableSchemaExactWithConfigAppliesPiJSONSchemaExtension(t *testing.T) {
	fakeBin := t.TempDir()
	wrapper := filepath.Join(fakeBin, "pi-wrapper")
	if runtime.GOOS == "windows" {
		wrapper += ".exe"
	}
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", t.TempDir())

	originalRegistry := registry
	registry = map[string]Agent{
		"pi": NewPiAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	resolved, err := GetAvailableSchemaExactWithConfig("pi", &config.Config{
		PiCmd: wrapper,
		Agent: config.AgentConfig{
			Pi: config.PiConfig{
				JSONSchemaExtension: "/opt/roborev/pi-json-schema/index.ts",
			},
		},
	})
	require.NoError(t, err)
	require.IsType(t, &PiAgent{}, resolved)
	pi := resolved.(*PiAgent)
	assert.Equal(t, wrapper, pi.CommandName())
	assert.Equal(t, "/opt/roborev/pi-json-schema/index.ts", pi.JSONSchemaExtension)
}

func TestGetAvailableSchemaWithConfigFallsBackToAvailableSchemaAgent(t *testing.T) {
	fakeBin := t.TempDir()
	codexBin := filepath.Join(fakeBin, "codex")
	if runtime.GOOS == "windows" {
		codexBin += ".exe"
	}
	require.NoError(t, os.WriteFile(codexBin, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"claude-code": NewClaudeAgent("definitely-not-on-path"),
		"codex": &fakeSchemaAgent{
			TestAgent: NewTestAgent(),
			name:      "codex",
			command:   "codex",
		},
	}
	t.Cleanup(func() { registry = originalRegistry })

	resolved, err := GetAvailableSchemaWithConfig("claude-code", nil)
	require.NoError(t, err)
	assert.Equal(t, "codex", resolved.Name())
}
