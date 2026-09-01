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

			assert.Equal(t, []string{
				"agent-hook", "run", "--agent", string(profile.Agent), agentHookMarker,
			}, opts.Arguments)
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
		{name: "quoted executable and agent", command: `"/opt/Roborev Dev" agent-hook run --agent "qwen"`, want: kitagenthook.AgentQwen},
		{name: "duplicate", command: "roborev agent-hook run --agent qwen --agent qwen", wantErr: "exactly one"},
		{name: "conflict", command: "roborev agent-hook run --agent qwen --agent gemini", wantErr: "exactly one"},
		{name: "missing value", command: "roborev agent-hook run --agent", wantErr: "requires a value"},
		{name: "empty value", command: "roborev agent-hook run --agent=", wantErr: "requires a value"},
		{name: "argument terminator", command: "roborev agent-hook run -- --agent qwen", wantErr: "argument terminator"},
		{name: "quoted data", command: `echo "roborev agent-hook run --agent qwen"`, wantErr: "must invoke"},
		{name: "chained command", command: "roborev agent-hook run --agent qwen; echo skipped", wantErr: "shell operator"},
		{name: "quoted invocation", command: `sh -c "roborev agent-hook run --agent qwen"`, wantErr: "must invoke"},
		{name: "quoted command substitution", command: `roborev agent-hook run --agent "$(agent)"`, wantErr: "shell operator"},
		{name: "quoted backticks", command: "roborev agent-hook run --agent \"`agent`\"", wantErr: "shell operator"},
		{name: "escaped newline substitution", command: "roborev agent-hook run --agent \"$\\\n(agent)\"", wantErr: "single command line"},
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
		Executable: "/opt/bin/custom-hook",
		ConfigPath: path,
		Timeout:    10 * time.Second,
	}, &stdout)

	require.NoError(t, err)
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), "agent-hook run --agent qwen")
	assert.Contains(t, string(body), agentHookMarker)
	assert.Contains(t, stdout.String(), "installed Qwen Code agent hooks")
}

func TestRunInstallInstallsAndUpdatesBundledSkillsForSupportedProfiles(t *testing.T) {
	tests := []struct {
		agent      string
		configName string
	}{
		{agent: "claude", configName: "settings.json"},
		{agent: "codex", configName: "hooks.json"},
		{agent: "droid", configName: "hooks.json"},
		{agent: "grok", configName: filepath.Join("hooks", "roborev.json")},
	}

	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, tt.configName)
			opts := InstallOptions{
				Agent: tt.agent, Executable: "/opt/bin/roborev",
				ConfigPath: configPath, Timeout: 10 * time.Second,
			}

			require.NoError(t, RunInstall(opts, &bytes.Buffer{}))
			skillPath := filepath.Join(root, "skills", "roborev-fix", "SKILL.md")
			installed, err := os.ReadFile(skillPath)
			require.NoError(t, err)
			assert.NotEmpty(t, installed)

			require.NoError(t, os.WriteFile(skillPath, []byte("stale"), 0o644))
			require.NoError(t, RunInstall(opts, &bytes.Buffer{}))
			updated, err := os.ReadFile(skillPath)
			require.NoError(t, err)
			assert.NotEqual(t, []byte("stale"), updated)
		})
	}
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

func TestRunInstallAddsOwnershipMarkerToCustomCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	err := RunInstall(InstallOptions{
		Agent:      "qwen",
		Command:    `"/opt/Roborev Dev" agent-hook run --agent "qwen"`,
		ConfigPath: path,
	}, &bytes.Buffer{})

	require.NoError(t, err)
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), agentHookMarker)
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
