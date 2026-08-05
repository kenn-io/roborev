package agenthook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestRunInstallMigratesLegacyProfileHooks(t *testing.T) {
	tests := []struct {
		agent      string
		legacy     string
		preserved  string
		configName string
	}{
		{
			agent:      "codex",
			legacy:     `'C:\Program Files\roborev.exe' agent-hook run --turn-threshold 3`,
			preserved:  `'C:\Program Files\roborev.exe' agent-hook run --agent droid`,
			configName: "hooks.json",
		},
		{
			agent:      "claude",
			legacy:     `/old/bin/roborev agent-hook run --config /tmp/roborev.toml`,
			preserved:  `/old/bin/roborev agent-hook run --agent droid`,
			configName: "settings.json",
		},
		{
			agent:      "droid",
			legacy:     `/old/bin/roborev agent-hook run --config /tmp/roborev.toml --agent=droid`,
			preserved:  `/old/bin/roborev agent-hook run`,
			configName: "hooks.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tt.configName)
			fixture, err := json.Marshal(map[string]any{
				"hooks": map[string]any{
					"Stop": []any{map[string]any{
						"hooks": []any{
							map[string]any{"type": "command", "command": tt.legacy},
							map[string]any{"type": "command", "command": tt.preserved},
						},
					}},
				},
			})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, fixture, 0o600))

			err = RunInstall(InstallOptions{
				Agent:      tt.agent,
				Executable: "/new/bin/roborev",
				ConfigPath: path,
				Timeout:    10 * time.Second,
			}, &bytes.Buffer{})
			require.NoError(t, err)

			body, err := os.ReadFile(path)
			require.NoError(t, err)
			var root map[string]any
			require.NoError(t, json.Unmarshal(body, &root))
			var commands []string
			var collectCommands func(any)
			collectCommands = func(value any) {
				switch typed := value.(type) {
				case map[string]any:
					if command, ok := typed["command"].(string); ok {
						commands = append(commands, command)
					}
					for _, child := range typed {
						collectCommands(child)
					}
				case []any:
					for _, child := range typed {
						collectCommands(child)
					}
				}
			}
			collectCommands(root["hooks"])

			assert.NotContains(t, commands, tt.legacy)
			assert.Contains(t, commands, tt.preserved)
			installed := 0
			for _, command := range commands {
				if strings.Contains(command, "agent-hook run --agent "+tt.agent) &&
					strings.Contains(command, agentHookMarker) {
					installed++
				}
			}
			assert.Equal(t, 3, installed)
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

func TestRunDumpMigratesLegacyHooksWithoutChangingSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	legacy := `/old/bin/roborev agent-hook run --turn-threshold 3`
	source, err := json.Marshal(map[string]any{
		"custom": "preserved",
		"hooks": map[string]any{
			"Stop": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": legacy}},
			}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, source, 0o600))
	var stdout bytes.Buffer

	err = RunDump(DumpOptions{
		Agent:      "codex",
		Executable: "/new/bin/roborev",
		ConfigPath: path,
		Timeout:    10 * time.Second,
	}, &stdout)

	require.NoError(t, err)
	assert.NotContains(t, stdout.String(), legacy)
	assert.Contains(t, stdout.String(), "agent-hook run --agent codex")
	assert.Contains(t, stdout.String(), `"custom": "preserved"`)
	unchanged, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, source, unchanged)
}
