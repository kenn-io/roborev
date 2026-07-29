package agenthook

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitagenthook "go.kenn.io/kit/agenthook"
)

func TestKitInstallOptionsUseProfileSpecificRunArguments(t *testing.T) {
	for _, profile := range kitagenthook.Profiles() {
		t.Run(string(profile.Agent), func(t *testing.T) {
			opts := kitInstallOptions(profile.Agent, InstallOptions{
				Executable: "/opt/bin/roborev",
				Timeout:    10 * time.Second,
			})

			assert.Equal(t, []string{"agent-hook", "run", "--agent", string(profile.Agent)}, opts.Arguments)
			assert.Equal(t, agentHookMarker, opts.Marker)
			assert.Len(t, opts.Hooks, 3)
		})
	}
}

func TestCommandAgentRequiresExactlyOneSelection(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    kitagenthook.Agent
		wantErr string
	}{
		{name: "space form", command: "roborev agent-hook run --agent qwen", want: kitagenthook.AgentQwen},
		{name: "equals form", command: "roborev agent-hook run --agent=qwen", want: kitagenthook.AgentQwen},
		{name: "duplicate", command: "roborev agent-hook run --agent qwen --agent qwen", wantErr: "exactly one"},
		{name: "conflict", command: "roborev agent-hook run --agent qwen --agent gemini", wantErr: "exactly one"},
		{name: "missing value", command: "roborev agent-hook run --agent", wantErr: "requires a value"},
		{name: "empty value", command: "roborev agent-hook run --agent=", wantErr: "requires a value"},
		{name: "after terminator", command: "roborev agent-hook run -- --agent qwen", wantErr: "must select an agent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := commandAgent(tt.command)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRunInstallUsesKitForQwen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	var stdout bytes.Buffer

	err := RunInstall(InstallOptions{
		Agent:      "qwen",
		Executable: "/opt/bin/roborev",
		ConfigPath: path,
		Timeout:    10 * time.Second,
	}, &stdout)

	require.NoError(t, err)
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), "agent-hook run --agent qwen")
	assert.Contains(t, stdout.String(), "installed Qwen Code agent hooks")
}

func TestRunInstallRejectsCommandForDifferentProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	err := RunInstall(InstallOptions{
		Agent:      "gemini",
		Command:    "roborev agent-hook run --agent qwen",
		ConfigPath: path,
	}, &bytes.Buffer{})

	require.Error(t, err)
	require.ErrorContains(t, err, "selects qwen, not gemini")
	_, statErr := os.Stat(path)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRunDumpWritesCompleteNativeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	var stdout bytes.Buffer

	err := RunDump(DumpOptions{
		Agent:      "qwen",
		Executable: "/opt/bin/roborev",
		ConfigPath: path,
		Timeout:    10 * time.Second,
	}, &stdout)

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "agent-hook run --agent qwen")
	_, statErr := os.Stat(path)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}
