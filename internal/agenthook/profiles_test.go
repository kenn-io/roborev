package agenthook

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitagenthook "go.kenn.io/kit/agenthook"
)

func TestSelectProfilesAutoDetectsExecutableOrConfigDirectory(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.MkdirAll(bin, 0o755))

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "local-app-data"))
	for name, dir := range map[string]string{
		"CODEX_HOME":        "codex",
		"CLAUDE_CONFIG_DIR": "claude",
		"COPILOT_HOME":      "copilot",
		"GEMINI_CLI_HOME":   "gemini",
		"HERMES_HOME":       "hermes",
		"QWEN_HOME":         "qwen",
	} {
		t.Setenv(name, filepath.Join(root, "agent-homes", dir))
	}

	gemini := filepath.Join(bin, "gemini")
	if runtime.GOOS == "windows" {
		gemini += ".bat"
	}
	require.NoError(t, os.WriteFile(gemini, []byte("exit 0\n"), 0o755))
	t.Setenv("PATH", bin)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agent-homes", "qwen"), 0o755))

	agents, err := SelectProfiles("")

	require.NoError(t, err)
	assert.Equal(t, []kitagenthook.Agent{
		kitagenthook.AgentGemini,
		kitagenthook.AgentQwen,
	}, agents)
}

func TestSelectProfilesAllUsesKitOrderThenGrok(t *testing.T) {
	agents, err := SelectProfiles("all")

	require.NoError(t, err)
	assert.Equal(t, []kitagenthook.Agent{
		kitagenthook.AgentClaude,
		kitagenthook.AgentCodex,
		kitagenthook.AgentCopilot,
		kitagenthook.AgentCursor,
		kitagenthook.AgentDroid,
		kitagenthook.AgentGemini,
		kitagenthook.AgentHermes,
		kitagenthook.AgentQwen,
		AgentGrok,
	}, agents)
}

func TestSelectProfilesDoesNotTreatGrokAgentAliasAsCursor(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.MkdirAll(bin, 0o755))

	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "local-app-data"))
	t.Setenv("PATH", bin)
	for _, name := range []string{
		"CODEX_HOME", "CLAUDE_CONFIG_DIR", "COPILOT_HOME",
		"GEMINI_CLI_HOME", "HERMES_HOME", "QWEN_HOME",
	} {
		t.Setenv(name, filepath.Join(root, "agent-homes", name))
	}

	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	grok := filepath.Join(bin, "grok"+extension)
	alias := filepath.Join(bin, "agent"+extension)
	require.NoError(t, os.WriteFile(grok, []byte("grok fixture"), 0o755))
	require.NoError(t, os.Link(grok, alias))

	agents, err := SelectProfiles("")

	require.NoError(t, err)
	assert.Equal(t, []kitagenthook.Agent{AgentGrok}, agents)
}

func TestSelectProfilesAutoReturnsActionableErrorWhenNothingDetected(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "local-app-data"))
	t.Setenv("PATH", filepath.Join(root, "bin"))
	for _, name := range []string{
		"CODEX_HOME", "CLAUDE_CONFIG_DIR", "COPILOT_HOME",
		"GEMINI_CLI_HOME", "HERMES_HOME", "QWEN_HOME",
	} {
		t.Setenv(name, filepath.Join(root, "agent-homes", name))
	}

	_, err := SelectProfiles("")

	require.Error(t, err)
	require.ErrorContains(t, err, "--agent")
	require.ErrorContains(t, err, "--agent all")
}
