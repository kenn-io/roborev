package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitagenthook "go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentsetup"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/mcpconfig"
	"go.kenn.io/roborev/internal/skills"
)

// runSetup runs roborev agent setup and returns its stdout.
func runSetup(args ...string) (string, error) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"agent", "setup"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

// fakeAgent puts this test binary on PATH as the named agent CLI.
func fakeAgent(t *testing.T, name string) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	t.Setenv("ROBOREV_TEST_FAKE_"+strings.ToUpper(name), "1")
	exe, err := os.Executable()
	require.NoError(t, err)
	data, err := os.ReadFile(exe)
	require.NoError(t, err)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(bin, name), data, 0o700))
}

// setupReport runs setup with --json and returns its rows by piece.
func setupReport(t *testing.T, args ...string) (map[string]agentsetup.State, error) {
	out, err := runSetup(append(args, "--json")...)
	var report agentsetup.Report
	require.NoError(t, json.Unmarshal([]byte(out), &report), out)
	states := map[string]agentsetup.State{}
	for _, row := range report.Rows {
		states[row.Piece] = row.State
	}
	return states, err
}

func readFile(t *testing.T, path string) string {
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func TestAgentSetupMCPReruns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	fakeAgent(t, "codex")
	t.Setenv("ROBOREV_TEST_HOOK_TRUST", "trusted")
	_, err := runSetup("--agent", "codex", "--mcp", "--server", "http://127.0.0.1:8181")
	require.NoError(t, err)
	path := filepath.Join(home, "config.toml")
	hooks := readFile(t, filepath.Join(home, "hooks.json"))
	config := readFile(t, path)
	out, err := runSetup("--agent", "codex", "--check", "--mcp=false", "--server", "http://127.0.0.1:8282")
	require.NoError(t, err, out)
	assert.Equal(t, hooks, readFile(t, filepath.Join(home, "hooks.json")))
	assert.Equal(t, config, readFile(t, path))

	config = strings.ReplaceAll(config, "http://127.0.0.1:8181", "http://127.0.0.1:8282")
	require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
	states, err := setupReport(t, "--agent", "codex", "--check")
	require.Error(t, err)
	assert.Equal(t, agentsetup.StateOutdated, states["mcp roborev"])
	_, err = runSetup("--agent", "codex")
	require.NoError(t, err)
	assert.Contains(t, readFile(t, path), "http://127.0.0.1:8181")
	installed, found, err := agenthook.InstalledOptions(kitagenthook.AgentCodex)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, installed.MCP)
	status, available := skills.StatusForAgentMode(skills.AgentCodex, new(true))
	require.True(t, available)
	for _, state := range status.Skills {
		assert.Equal(t, skills.SkillCurrent, state)
	}

	out, err = runSetup("--agent", "codex", "--mcp", "--server", "http://127.0.0.1:8282")
	require.NoError(t, err, out)
	installed, found, err = agenthook.InstalledOptions(kitagenthook.AgentCodex)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "http://127.0.0.1:8282", installed.RoborevServerAddr)
	assert.True(t, installed.MCP)
	registration, found := mcpconfig.Registration(mcpconfig.Options{Agent: skills.AgentCodex})
	require.True(t, found)
	assert.Contains(t, registration.Args, "http://127.0.0.1:8282")
	_, err = runSetup("--agent", "codex", "--mcp=false")
	require.NoError(t, err)
	installed, _, err = agenthook.InstalledOptions(kitagenthook.AgentCodex)
	require.NoError(t, err)
	assert.False(t, installed.MCP)
}

func TestAgentSetupKeepsCustomCommands(t *testing.T) {
	t.Run("explicit mode switches custom command", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		fakeAgent(t, "claude")
		exe, err := os.Executable()
		require.NoError(t, err)
		command := fmt.Sprintf("%s agent-hook run --agent claude --mcp=true --turn-threshold 7", exe)
		require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Command: command, MCP: true}, io.Discard))
		out, err := runSetup("--agent", "claude", "--mcp=false")
		require.NoError(t, err, out)
		installed, found, err := agenthook.InstalledOptions(kitagenthook.AgentClaude)
		require.NoError(t, err)
		require.True(t, found)
		assert.False(t, installed.MCP)
		assert.Contains(t, installed.Command, "--turn-threshold")
		assert.NotContains(t, installed.Command, "--mcp")
		out, err = runSetup("--agent", "claude", "--mcp")
		require.NoError(t, err, out)
		installed, found, err = agenthook.InstalledOptions(kitagenthook.AgentClaude)
		require.NoError(t, err)
		require.True(t, found)
		assert.True(t, installed.MCP)
		assert.Contains(t, installed.Command, "--turn-threshold")
	})
	t.Run("missing custom executable", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", home)
		fakeAgent(t, "claude")
		missing := filepath.Join(t.TempDir(), "roborev")
		command := fmt.Sprintf("%s agent-hook run --agent claude --turn-threshold 0", missing)
		require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Command: command}, io.Discard))
		_, err := runSetup("--agent", "claude", "--mcp")
		require.ErrorIs(t, err, agentsetup.ErrNotReady)
		installed, _, err := agenthook.InstalledOptions(kitagenthook.AgentClaude)
		require.NoError(t, err)
		assert.Equal(t, missing, installed.Executable)
		registration, found := mcpconfig.Registration(mcpconfig.Options{Agent: skills.AgentClaude})
		require.True(t, found)
		assert.Equal(t, missing, registration.Command)
	})
}

func TestAgentSetupKeepsMCPExecutableWhenHookMoves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	missing := filepath.Join(t.TempDir(), "roborev")
	require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Executable: missing, MCP: true, RoborevServerAddr: "http://127.0.0.1:8181"}, io.Discard))
	path := filepath.Join(home, ".claude.json")
	var config map[string]any
	require.NoError(t, json.Unmarshal([]byte(readFile(t, path)), &config))
	entry := config["mcpServers"].(map[string]any)["roborev"].(map[string]any)
	entry["env"] = map[string]any{"TOKEN": "example"}
	data, err := json.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	out, err := runSetup("--agent", "claude", "--mcp")
	require.NoError(t, err, out)
	exe, err := os.Executable()
	require.NoError(t, err)
	installed, found, err := agenthook.InstalledOptions(kitagenthook.AgentClaude)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, exe, installed.Executable)
	registration, found := mcpconfig.Registration(mcpconfig.Options{Agent: skills.AgentClaude})
	require.True(t, found)
	assert.Equal(t, missing, registration.Command)
	assert.Equal(t, []string{"--server", "http://127.0.0.1:8181", "mcp", "serve"}, registration.Args)
	assert.Equal(t, map[string]string{"TOKEN": "example"}, registration.Env)
}

func TestQuickstartJSONReportsUntrustedCodex(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	fakeAgent(t, "codex")
	t.Setenv("ROBOREV_TEST_HOOK_TRUST", "untrusted")
	previousServer, previousEndpoint := serverAddr, parsedServerEndpoint
	serverAddr, parsedServerEndpoint = "unix://"+filepath.Join(t.TempDir(), "daemon.sock"), nil
	t.Cleanup(func() { serverAddr, parsedServerEndpoint = previousServer, previousEndpoint })
	root := newRootCmd()
	var setupOut, stderr bytes.Buffer
	root.SetOut(&setupOut)
	root.SetErr(&stderr)
	assert.Equal(t, 1, executeCLI(root, []string{"agent", "setup", "--agent", "codex"}))
	assert.Empty(t, stderr.String())
	cmd := quickstartCmd()
	cmd.SetArgs([]string{"--json"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, cmd.Execute())
	var state quickstartState
	require.NoError(t, json.Unmarshal(out.Bytes(), &state))
	check := quickstartCheckByID(t, state, "agent_hook_codex")
	assert.Equal(t, checkStatus("untrusted"), check.Status)
	assert.Contains(t, check.FixCommand, "/hooks")
}

func TestReadinessUsesInstalledHookModeAndSuggestsMCPRepair(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("PATH", t.TempDir())
	_, err := mcpconfig.Install(mcpconfig.Options{Agent: skills.AgentClaude})
	require.NoError(t, err)
	require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Executable: "roborev"}, io.Discard))
	_, details := agentReadiness(t.Context(), "claude")
	require.Len(t, details, 2)
	assert.Contains(t, details[0], "hooks:")
	assert.Contains(t, details[1], "skills:")
	assert.NotContains(t, strings.Join(details, " "), "mcp roborev")
	require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Executable: "roborev", MCP: true}, io.Discard))
	require.NoError(t, os.Remove(filepath.Join(home, ".claude.json")))
	states, err := setupReport(t, "--agent", "claude", "--check")
	require.Error(t, err)
	assert.Equal(t, agentsetup.StateMissing, states["mcp roborev"])
}

func TestAgentSetupPartialMCPInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	require.NoError(t, os.WriteFile(filepath.Join(home, "skills"), []byte("blocks skills"), 0o600))
	_, err := runSetup("--agent", "claude", "--mcp", "--server", "http://127.0.0.1:8282")
	require.Error(t, err)
	registration := readFile(t, filepath.Join(home, ".claude.json"))
	assert.Contains(t, registration, `"serve"`)
	assert.Contains(t, registration, "http://127.0.0.1:8282")
	require.NoError(t, os.Remove(filepath.Join(home, "skills")))
	states, err := setupReport(t, "--agent", "claude", "--mcp", "--server", "http://127.0.0.1:8282")
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateOK, states["skills"])
}

func TestSetupPrintUsesSelectedServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("PATH", t.TempDir())
	out, err := runSetup("--server", "http://127.0.0.1:8181", "--agent", "claude", "--print")
	require.NoError(t, err)
	assert.Contains(t, out, "http://127.0.0.1:8181")
	require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Executable: "roborev", RoborevServerAddr: "http://127.0.0.1:8282"}, io.Discard))
	out, err = runSetup("--agent", "claude", "--print")
	require.NoError(t, err)
	assert.NotContains(t, out, "http://127.0.0.1:8282")
	path := filepath.Join(home, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	_, err = runSetup("--agent", "claude", "--print", "--server", "http://127.0.0.1:8181")
	require.NoError(t, err)
	assert.Equal(t, "{", readFile(t, path))
}

func TestDoctorAgentReach(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	fakeAgent(t, "claude")
	checks := checkDoctorAgentReach(&doctorEnv{ctx: t.Context()})
	require.Len(t, checks, 1)
	assert.Equal(t, "agents.reach.claude", checks[0].ID)
	assert.Equal(t, doctorWarn, checks[0].Status)
	assert.Contains(t, strings.Join(checks[0].Details, "\n"), "hooks")
	require.NoError(t, os.WriteFile(filepath.Join(claude, "settings.json"), []byte("{"), 0o600))
	checks = checkDoctorAgentReach(&doctorEnv{ctx: t.Context()})
	assert.Equal(t, doctorFail, checks[0].Status)
}

func fakeCodexAppServer() int {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	path := filepath.Join(os.Getenv("CODEX_HOME"), "hooks.json")
	for scanner.Scan() {
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return 1
		}
		var result any
		switch request.Method {
		case "initialized":
			continue
		case "initialize":
			result = map[string]any{}
		case "config/read":
			result = map[string]any{"config": map[string]any{"features": map[string]bool{"hooks": true}}}
		case "hooks/list":
			var hooks []map[string]any
			for _, event := range []string{"pretooluse", "posttooluse", "stop"} {
				hooks = append(hooks, map[string]any{"key": path + ":" + event + ":0:0", "source": "user", "sourcePath": path, "enabled": true, "trustStatus": os.Getenv("ROBOREV_TEST_HOOK_TRUST")})
			}
			result = map[string]any{"data": []any{map[string]any{"hooks": hooks}}}
		case "mcpServerStatus/list":
			result = map[string]any{"data": []any{map[string]any{"name": "roborev", "tools": map[string]any{"read_review": map[string]any{}}}}}
		default:
			return 2
		}
		if encoder.Encode(map[string]any{"id": request.ID, "result": result}) != nil {
			return 3
		}
	}
	return 0
}

func TestSetupLeavesMixedHookCommandsUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	exe, err := os.Executable()
	require.NoError(t, err)
	require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Executable: exe, MCP: true, RoborevServerAddr: "http://127.0.0.1:8181"}, io.Discard))
	path := filepath.Join(home, "settings.json")
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(readFile(t, path)), &doc))
	hooks := doc["hooks"].(map[string]any)
	handler := hooks["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	handler["command"] = handler["command"].(string) + " --turn-threshold 7"
	handler["timeout"] = float64(23)
	data, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	mcpPath := filepath.Join(home, ".claude.json")
	mcp := readFile(t, mcpPath)
	states, err := setupReport(t, "--agent", "claude", "--server", "http://127.0.0.1:8282")
	require.ErrorIs(t, err, agentsetup.ErrNotReady)
	assert.Equal(t, agentsetup.StateUnknown, states["hooks"])
	assert.Equal(t, agentsetup.StateUnknown, states["mcp roborev"])
	assert.Equal(t, agentsetup.StateOK, states["skills"])
	assert.Equal(t, string(data), readFile(t, path))
	assert.Equal(t, mcp, readFile(t, mcpPath))
}

func TestSetupContinuesAfterOtherAgentConfigFails(t *testing.T) {
	claude := t.TempDir()
	codex := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CODEX_HOME", codex)
	fakeAgent(t, "codex")
	t.Setenv("ROBOREV_TEST_HOOK_TRUST", "trusted")
	require.NoError(t, os.WriteFile(filepath.Join(claude, "settings.json"), []byte("{"), 0o600))
	out, err := runSetup("--agent", "claude,codex", "--json")
	require.Error(t, err)
	var report agentsetup.Report
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Contains(t, readFile(t, filepath.Join(codex, "hooks.json")), "roborev-agent-hook")
	assert.Equal(t, "{", readFile(t, filepath.Join(claude, "settings.json")))
}

func TestSetupPreservesEventSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	executable, err := os.Executable()
	require.NoError(t, err)
	require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Executable: executable}, io.Discard))
	path := filepath.Join(home, "settings.json")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	hooks := doc["hooks"].(map[string]any)
	group := hooks["PreToolUse"].([]any)[0].(map[string]any)
	group["matcher"] = "Bash|Write"
	group["hooks"].([]any)[0].(map[string]any)["timeout"] = float64(23)
	delete(hooks, "Stop")
	data, err = json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	out, err := runSetup("--agent", "claude", "--server", "http://127.0.0.1:8282")
	require.NoError(t, err, out)
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &doc))
	hooks = doc["hooks"].(map[string]any)
	group = hooks["PreToolUse"].([]any)[0].(map[string]any)
	assert.Equal(t, "Bash|Write", group["matcher"])
	assert.Equal(t, float64(23), group["hooks"].([]any)[0].(map[string]any)["timeout"])
	assert.Contains(t, group["hooks"].([]any)[0].(map[string]any)["command"], "http://127.0.0.1:8282")
	require.Contains(t, hooks, "Stop")
	stop := hooks["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	assert.Contains(t, stop["command"], "http://127.0.0.1:8282")
}
