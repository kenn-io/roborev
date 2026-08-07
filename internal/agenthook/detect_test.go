package agenthook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitagenthook "go.kenn.io/kit/agenthook"
)

func TestInstalledMissingFile(t *testing.T) {
	ok, err := Installed(kitagenthook.AgentClaude, filepath.Join(t.TempDir(), "nope.json"))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestInstalledDetectsRoborevHook(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	content := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"roborev agent-hook run --source=roborev-agent-hook"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	ok, err := Installed(kitagenthook.AgentClaude, path)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestInstalledForAgentDetectsGrokHook(t *testing.T) {
	for _, command := range []string{
		`roborev agent-hook run --agent grok`,
		`roborev agent-hook run --agent=grok`,
		`roborev agent-hook run --agent "grok"`,
	} {
		t.Run(command, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hooks.json")
			content, err := json.Marshal(map[string]any{
				"hooks": map[string]any{"Stop": []any{map[string]any{
					"hooks": []any{map[string]any{"type": "command", "command": command}},
				}}},
			})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, content, 0o644))

			ok, err := InstalledForAgent(path, "grok")
			require.NoError(t, err)
			assert.True(t, ok)
		})
	}
}

func TestInstalledIgnoresUnrelatedHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	content := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo hi"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	ok, err := Installed(kitagenthook.AgentClaude, path)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestInstalledInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))
	_, err := Installed(kitagenthook.AgentClaude, path)
	assert.Error(t, err)
}
