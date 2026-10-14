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
)

func zcodeInstallFixture(t *testing.T) (configPath, pluginRoot string) {
	t.Helper()
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	root := t.TempDir()
	path := filepath.Join(root, "cli", "config.json")
	legacyMarked := "/opt/roborev agent-hook run --agent zcode --source=roborev-agent-hook"
	legacyUnowned := "/opt/user-hook agent-hook run --agent zcode"
	fixture, err := json.Marshal(map[string]any{
		"plugins": map[string]any{"enabledPlugins": map[string]any{"demo": true}},
		"hooks": map[string]any{
			"enabled": true,
			"events": map[string]any{"Stop": []any{map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": legacyMarked},
					map[string]any{"type": "command", "command": legacyUnowned},
				},
			}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, fixture, 0o600))
	return path, zcodePluginRoot(path)
}

func TestRunInstallZcodeRegistersPluginAndCleansLegacyHooks(t *testing.T) {
	assert := assert.New(t)
	path, pluginRoot := zcodeInstallFixture(t)
	opts := InstallOptions{
		Agent: "zcode", Executable: "/opt/bin/roborev",
		ConfigPath: path, Timeout: 10 * time.Second,
	}
	var first, second bytes.Buffer

	require.NoError(t, RunInstall(opts, &first))
	require.NoError(t, RunInstall(opts, &second))

	assert.Contains(first.String(), "installed ZCode agent hooks")
	assert.Contains(second.String(), "ZCode agent hooks already installed")

	manifest, err := os.ReadFile(filepath.Join(pluginRoot, ".zcode-plugin", "plugin.json"))
	require.NoError(t, err)
	var pluginManifest map[string]any
	require.NoError(t, json.Unmarshal(manifest, &pluginManifest))
	assert.Equal("roborev", pluginManifest["name"])
	assert.Equal("hooks", pluginManifest["hooks"])

	hooksBody, err := os.ReadFile(filepath.Join(pluginRoot, "hooks", "hooks.json"))
	require.NoError(t, err)
	var hooksDoc map[string]any
	require.NoError(t, json.Unmarshal(hooksBody, &hooksDoc))
	events, ok := hooksDoc["hooks"].(map[string]any)
	require.True(t, ok)
	// ZCode schedules only UserPromptSubmit and SessionStart; Stop stays
	// registered so it activates on its own once ZCode dispatches it.
	assert.Len(events, 3)
	assert.Contains(events, "UserPromptSubmit")
	assert.Contains(events, "SessionStart")
	assert.Contains(events, "Stop")
	assert.Contains(string(hooksBody), agentHookMarker)
	installed := 0
	for _, rawEntries := range events {
		for _, rawEntry := range rawEntries.([]any) {
			for _, rawHandler := range rawEntry.(map[string]any)["hooks"].([]any) {
				command := rawHandler.(map[string]any)["command"].(string)
				handler := rawHandler.(map[string]any)
				assert.InDelta(float64(30), handler["timeout"], 0.001)
				if strings.Contains(command, agentHookMarker) {
					installed++
				}
			}
		}
	}
	assert.Equal(3, installed)

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, json.Unmarshal(body, &cfg))
	plugins, ok := cfg["plugins"].(map[string]any)
	require.True(t, ok)
	assert.Equal([]any{pluginRoot}, plugins["dirs"])
	enabledPlugins, ok := plugins["enabledPlugins"].(map[string]any)
	require.True(t, ok)
	assert.Contains(enabledPlugins, "demo")
	hooks, ok := cfg["hooks"].(map[string]any)
	require.True(t, ok)
	// The legacy configuration-hook form is not loaded by ZCode, so the
	// install must drop roborev-owned entries while preserving foreign ones.
	legacyEvents, ok := hooks["events"].(map[string]any)
	require.True(t, ok)
	stopEntries := legacyEvents["Stop"].([]any)
	require.Len(t, stopEntries, 1)
	keptCommand := stopEntries[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]
	assert.Equal("/opt/user-hook agent-hook run --agent zcode", keptCommand)
	assert.NotContains(string(body), "/opt/roborev agent-hook run --agent zcode --source=roborev-agent-hook")
}

func TestRunDumpZcodeDoesNotWriteSource(t *testing.T) {
	assert := assert.New(t)
	path, _ := zcodeInstallFixture(t)
	source, err := os.ReadFile(path)
	require.NoError(t, err)
	var stdout bytes.Buffer

	err = RunDump(InstallOptions{
		Agent: "zcode", Executable: "/opt/bin/roborev",
		ConfigPath: path, Timeout: 10 * time.Second,
	}, &stdout)

	require.NoError(t, err)
	var dump map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &dump))
	// The dump describes the full installation: the registration config
	// plus each plugin file with its destination, so applying it never
	// registers a plugin whose files are missing.
	assert.Equal(path, dump["config_path"])
	files, ok := dump["files"].(map[string]any)
	require.True(t, ok)
	assert.Len(files, 2)
	for dest, body := range files {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		assert.NotEmpty(t, encoded)
		assert.Contains(dest, "zcode-plugin")
	}
	hooksDoc, ok := dump["config"].(map[string]any)
	require.True(t, ok)
	assert.Contains(hooksDoc, "plugins")
	unchanged, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(source, unchanged)
	_, statErr := os.Stat(filepath.Join(zcodePluginRoot(path), "hooks", "hooks.json"))
	assert.ErrorIs(statErr, os.ErrNotExist)
}

func TestRunInstallZcodeStopFormRegistersLifecycleEvents(t *testing.T) {
	assert := assert.New(t)
	path, pluginRoot := zcodeInstallFixture(t)
	opts := InstallOptions{
		Agent: "zcode", Executable: "/opt/bin/roborev", StopForm: true,
		ConfigPath: path, Timeout: 10 * time.Second,
	}

	require.NoError(t, RunInstall(opts, &bytes.Buffer{}))

	hooksBody, err := os.ReadFile(filepath.Join(pluginRoot, "hooks", "hooks.json"))
	require.NoError(t, err)
	var hooksDoc map[string]any
	require.NoError(t, json.Unmarshal(hooksBody, &hooksDoc))
	events, ok := hooksDoc["hooks"].(map[string]any)
	require.True(t, ok)
	assert.Len(events, 3)
	assert.Contains(string(hooksBody), ZcodeShellMatcher)
	for _, event := range []string{"PreToolUse", "PostToolUse", "Stop"} {
		assert.Contains(events, event)
	}
}

func TestRunInstallZcodeScopesPluginDirectoryToConfig(t *testing.T) {
	assert := assert.New(t)
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	first, second := filepath.Join(t.TempDir(), "cli", "config.json"), filepath.Join(t.TempDir(), "cli", "config.json")
	for _, path := range []string{first, second} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o600))
	}

	require.NoError(t, RunInstall(InstallOptions{
		Agent: "zcode", Executable: "/opt/first/roborev",
		ConfigPath: first, Timeout: 10 * time.Second,
	}, &bytes.Buffer{}))
	require.NoError(t, RunInstall(InstallOptions{
		Agent: "zcode", Executable: "/opt/second/roborev",
		ConfigPath: second, Timeout: 10 * time.Second,
	}, &bytes.Buffer{}))

	firstHooks, err := os.ReadFile(filepath.Join(zcodePluginRoot(first), "hooks", "hooks.json"))
	require.NoError(t, err)
	assert.Contains(string(firstHooks), "/opt/first/roborev")
	secondHooks, err := os.ReadFile(filepath.Join(zcodePluginRoot(second), "hooks", "hooks.json"))
	require.NoError(t, err)
	assert.Contains(string(secondHooks), "/opt/second/roborev")
	assert.NotEqual(zcodePluginRoot(first), zcodePluginRoot(second))
}
