package mcpconfig

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"go.kenn.io/roborev/internal/skills"
)

func TestInstallMergesAndSwitchesTransports(t *testing.T) {
	for _, agent := range skills.Agents() {
		t.Run(string(agent), func(t *testing.T) {
			assert := assert.New(t)
			dir := t.TempDir()
			opts := Options{Agent: agent, ConfigDir: dir, Executable: "/opt/bin/roborev", Transport: "stdio", DryRun: true}
			plan, err := Install(opts)
			require.NoError(t, err)
			initial := `{"theme":"dark","mcpServers":{"other":{"command":"other"}}}`
			key := "mcpServers"
			switch agent {
			case skills.AgentCodex, skills.AgentGrok:
				initial = "theme = 'dark'\n[mcp_servers.other]\ncommand = 'other'\n"
				key = "mcp_servers"
			case skills.AgentHermes:
				initial = "theme: dark\nmcp_servers:\n  other:\n    command: other\n"
				key = "mcp_servers"
			}
			require.NoError(t, os.WriteFile(plan.Path, []byte(initial), 0o600))
			opts.DryRun = false
			for _, transport := range []string{"stdio", "http", "stdio"} {
				opts.Transport = transport
				opts.URL = ""
				if transport == "http" {
					opts.URL = "http://127.0.0.1:7373/mcp"
				}
				result, err := Install(opts)
				require.NoError(t, err)
				data, err := os.ReadFile(result.Path)
				require.NoError(t, err)
				var doc map[string]any
				switch agent {
				case skills.AgentCodex, skills.AgentGrok:
					err = toml.Unmarshal(data, &doc)
				case skills.AgentHermes:
					err = yaml.Unmarshal(data, &doc)
				default:
					err = json.Unmarshal(data, &doc)
				}
				require.NoError(t, err)
				assert.Equal("dark", doc["theme"])
				servers := doc[key].(map[string]any)
				assert.Equal("other", servers["other"].(map[string]any)["command"])
				entry := servers["roborev"].(map[string]any)
				if transport == "stdio" {
					assert.Equal(opts.Executable, entry["command"])
					assert.Equal([]any{"mcp", "serve"}, entry["args"])
					assert.NotContains(entry, "url")
					assert.NotContains(entry, "httpUrl")
				} else {
					urlKey := "url"
					if agent == skills.AgentGemini || agent == skills.AgentQwen {
						urlKey = "httpUrl"
					}
					assert.Equal(opts.URL, entry[urlKey])
					assert.NotContains(entry, "command")
				}
				repeat, err := Install(opts)
				require.NoError(t, err)
				assert.False(repeat.Changed)
			}
		})
	}
}

func TestInstallDryRunAndInvalidInputPreserveConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	initial := []byte(`{"mcpServers":{"other":{"command":"other"}}}`)
	require.NoError(t, os.WriteFile(path, initial, 0o600))
	opts := Options{Agent: skills.AgentDroid, ConfigPath: path, Transport: "stdio", DryRun: true}
	result, err := Install(opts)
	require.NoError(t, err)
	assert.True(t, result.Changed)
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, initial, actual)
	opts.DryRun = false
	opts.Transport = "http"
	_, err = Install(opts)
	require.ErrorContains(t, err, "requires")
	actual, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, initial, actual)
}

func TestInstallWhitespaceOnlyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(" \n\t"), 0o600))
	_, err := Install(Options{Agent: skills.AgentDroid, ConfigPath: path})
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	assert.Contains(t, doc["mcpServers"], "roborev")
}

func TestInstallPreservesConfigSymlinkAndMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "shared.json")
	path := filepath.Join(dir, "mcp.json")
	require.NoError(t, os.WriteFile(target, []byte(`{"theme":"dark"}`), 0o640))
	require.NoError(t, os.Symlink(target, path))
	_, err := Install(Options{Agent: skills.AgentDroid, ConfigPath: path})
	require.NoError(t, err)
	link, err := os.Readlink(path)
	require.NoError(t, err)
	assert.Equal(t, target, link)
	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	assert.Equal(t, "dark", doc["theme"])
	assert.Contains(t, doc["mcpServers"], "roborev")
}
