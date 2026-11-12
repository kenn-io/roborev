package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitagenthook "go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentsetup"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/mcpconfig"
	"go.kenn.io/roborev/internal/skills"
)

// runSetup runs roborev agent setup and returns its stdout.
func runSetup(args ...string) (string, error) {
	previousServer, previousEndpoint := serverAddr, parsedServerEndpoint
	defer func() { serverAddr, parsedServerEndpoint = previousServer, previousEndpoint }()
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
	previousServer, previousEndpoint := serverAddr, parsedServerEndpoint
	_, err := runSetup("--agent", "codex", "--mcp", "--server", "http://127.0.0.1:8181")
	require.NoError(t, err)
	assert.Equal(t, previousServer, serverAddr)
	assert.Equal(t, previousEndpoint, parsedServerEndpoint)
	path := filepath.Join(home, "config.toml")
	hooks := readFile(t, filepath.Join(home, "hooks.json"))
	config := readFile(t, path)
	out, err := runSetup("--agent", "codex", "--check")
	require.NoError(t, err, out)
	assert.Equal(t, hooks, readFile(t, filepath.Join(home, "hooks.json")))
	assert.Equal(t, config, readFile(t, path))

	config = strings.ReplaceAll(config, "http://127.0.0.1:8181", "http://127.0.0.1:8282")
	require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
	states, err := setupReport(t, "--agent", "codex", "--check")
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateOK, states["mcp roborev"])
	_, err = runSetup("--agent", "codex")
	require.NoError(t, err)
	assert.Contains(t, readFile(t, path), "http://127.0.0.1:8282")
	assert.Contains(t, readFile(t, filepath.Join(home, "hooks.json")), "--mcp")
	status, available := skills.StatusForAgentMode(skills.AgentCodex, new(true))
	require.True(t, available)
	for _, state := range status.Skills {
		assert.Equal(t, skills.SkillCurrent, state)
	}

	out, err = runSetup("--agent", "codex", "--mcp", "--server", "http://127.0.0.1:8282")
	require.NoError(t, err, out)
	assert.Contains(t, readFile(t, filepath.Join(home, "hooks.json")), "http://127.0.0.1:8282")
	registration, found, err := mcpconfig.Registration(mcpconfig.Options{Agent: skills.AgentCodex})
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, registration.Args, "http://127.0.0.1:8282")
}

func TestSetupRejectsCheckChanges(t *testing.T) {
	for _, args := range [][]string{{"--check", "--mcp"}, {"--check", "--mcp=false"}, {"--check", "--server", "http://127.0.0.1:8282"}} {
		_, err := runSetup(append([]string{"--agent", "claude"}, args...)...)
		assert.ErrorContains(t, err, "--check cannot be combined")
	}
}

func TestSetupPreservesRegistrationWithoutPinningHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	exe, err := os.Executable()
	require.NoError(t, err)
	path := filepath.Join(home, ".claude.json")
	_, err = mcpconfig.Install(mcpconfig.Options{Agent: skills.AgentClaude, Executable: exe, Server: "http://127.0.0.1:8181"})
	require.NoError(t, err)
	before := readFile(t, path)
	out, err := runSetup("--agent", "claude", "--mcp")
	require.NoError(t, err, out)
	assert.Equal(t, before, readFile(t, path))
	assert.NotContains(t, readFile(t, filepath.Join(home, "settings.json")), "--roborev-server")
}

func TestSetupRejectsMalformedRegistration(t *testing.T) {
	for _, tc := range []struct{ agent, env, filename, config string }{
		{"claude", "CLAUDE_CONFIG_DIR", ".claude.json", `{"mcpServers":{"roborev":{"command":"custom","args":"bad","env":{"TOKEN":"example"}}}}`},
		{"codex", "CODEX_HOME", "config.toml", "[mcp_servers.roborev]\ncommand = 'custom'\nargs = 'bad'\n[mcp_servers.roborev.env]\nTOKEN = 'example'\n"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv(tc.env, home)
			fakeAgent(t, tc.agent)
			path := filepath.Join(home, tc.filename)
			require.NoError(t, os.WriteFile(path, []byte(tc.config), 0o600))
			_, err := runSetup("--agent", tc.agent, "--mcp")
			require.Error(t, err)
			assert.Equal(t, tc.config, readFile(t, path))
		})
	}
}

func TestSetupLeavesUnsupportedHooksUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"matcher", func(hooks map[string]any) { hooks["PreToolUse"].([]any)[0].(map[string]any)["matcher"] = "Write" }},
		{"event", func(hooks map[string]any) { hooks["UnknownEvent"] = hooks["Stop"]; delete(hooks, "Stop") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", home)
			fakeAgent(t, "claude")
			exe, err := os.Executable()
			require.NoError(t, err)
			require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Executable: exe, MCP: true}, io.Discard))
			path := filepath.Join(home, "settings.json")
			var doc map[string]any
			require.NoError(t, json.Unmarshal([]byte(readFile(t, path)), &doc))
			tc.mutate(doc["hooks"].(map[string]any))
			data, err := json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o600))
			out, err := runSetup("--agent", "claude", "--server", "http://127.0.0.1:8282")
			require.Error(t, err, out)
			assert.Equal(t, string(data), readFile(t, path))
			states, err := setupReport(t, "--agent", "claude", "--check")
			require.Error(t, err)
			assert.Equal(t, agentsetup.StateUnknown, states["hooks"])
			assert.Equal(t, agentsetup.StateUnknown, states["mcp roborev"])
		})
	}
}

func TestSetupChecksStdioMCPIdentity(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	data, err := os.ReadFile(exe)
	require.NoError(t, err)
	name := "roborev"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	copyPath := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(copyPath, data, 0o700))
	for _, tc := range []struct {
		command string
		want    agentsetup.State
	}{
		{"other-server", agentsetup.StateUnknown},
		{copyPath, agentsetup.StateOK},
	} {
		t.Run(filepath.Base(tc.command), func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			fakeAgent(t, "claude")
			opts := mcpconfig.Options{Agent: skills.AgentClaude, Executable: tc.command}
			_, err := mcpconfig.Install(opts)
			require.NoError(t, err)
			server, found, err := mcpconfig.Registration(opts)
			require.NoError(t, err)
			require.True(t, found)
			item, err := agentMCPItem("claude", exe, "", server, true)
			require.NoError(t, err)
			status, err := item.Check(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.want, status.State)
		})
	}
}

func TestSetupChecksAdvertisedHTTPMCP(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fakeAgent(t, "claude")
	advertised := "http://127.0.0.1:7373/mcp"
	stubMCPStatusDiscovery(t, []*daemon.RuntimeInfo{{PID: 11, Network: "tcp", Address: "127.0.0.1:7373"}}, func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return &daemon.PingInfo{PID: 11, MCPURL: advertised}, nil
	})
	opts := mcpconfig.Options{Agent: skills.AgentClaude, Transport: "http", URL: advertised}
	_, err := mcpconfig.Install(opts)
	require.NoError(t, err)
	server, _, err := mcpconfig.Registration(opts)
	require.NoError(t, err)
	item, err := agentMCPItem("claude", "roborev", "", server, true)
	require.NoError(t, err)
	status, err := item.Check(t.Context())
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateOK, status.State)
	advertised = "http://127.0.0.1:7374/mcp"
	status, err = item.Check(t.Context())
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateUnknown, status.State)
	assert.Contains(t, status.NextStep, "roborev mcp status")
	item, err = agentMCPItem("claude", "roborev", "http://127.0.0.1:7374", server, true)
	require.NoError(t, err)
	status, err = item.Check(t.Context())
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateOutdated, status.State)
}

func TestClaudeMCPUsesKitCheck(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fakeAgent(t, "claude")
	exe, err := os.Executable()
	require.NoError(t, err)
	opts := mcpconfig.Options{Agent: skills.AgentClaude, Executable: exe}
	result, err := mcpconfig.Install(opts)
	require.NoError(t, err)
	server, found, err := mcpconfig.Registration(opts)
	require.NoError(t, err)
	require.True(t, found)
	item, err := agentMCPItem("claude", exe, "", server, true)
	require.NoError(t, err)
	status, err := item.Check(t.Context())
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateOK, status.State)
	assert.Equal(t, result.Path, status.ConfigPath)

	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(readFile(t, result.Path)), &doc))
	doc["mcpServers"].(map[string]any)["roborev"].(map[string]any)["extra"] = "preserved"
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(result.Path, data, 0o600))
	status, err = item.Check(t.Context())
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateUnknown, status.State)
	assert.Equal(t, string(data), readFile(t, result.Path))

	doc["mcpServers"].(map[string]any)["roborev"].(map[string]any)["headersHelper"] = "helper"
	data, err = json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(result.Path, data, 0o600))
	server, _, err = mcpconfig.Registration(opts)
	require.NoError(t, err)
	item, err = agentMCPItem("claude", exe, "", server, true)
	require.NoError(t, err)
	status, err = item.Check(t.Context())
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateUnknown, status.State)
	assert.Equal(t, string(data), readFile(t, result.Path))
}

func TestAgentSetupKeepsCustomCommands(t *testing.T) {
	t.Run("explicit mode switches custom command", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		fakeAgent(t, "claude")
		exe, err := os.Executable()
		require.NoError(t, err)
		command, err := kitagenthook.BuildCommand(exe, "agent-hook", "run", "--agent", "claude", "--mcp=true", "--turn-threshold", "7")
		require.NoError(t, err)
		require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Command: command.POSIX, MCP: true}, io.Discard))
		out, err := runSetup("--agent", "claude", "--mcp=false")
		require.NoError(t, err, out)
		hooks := readFile(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"))
		assert.Contains(t, hooks, "--turn-threshold 7")
		assert.NotContains(t, hooks, "--mcp")
		out, err = runSetup("--agent", "claude", "--mcp")
		require.NoError(t, err, out)
		hooks = readFile(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"))
		assert.Contains(t, hooks, "--turn-threshold 7")
		assert.Contains(t, hooks, "--mcp")
	})
	t.Run("missing custom executable", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", home)
		fakeAgent(t, "claude")
		missing := filepath.Join(t.TempDir(), "roborev")
		command, err := kitagenthook.BuildCommand(missing, "agent-hook", "run", "--agent", "claude", "--turn-threshold", "0")
		require.NoError(t, err)
		require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Command: command.POSIX}, io.Discard))
		_, err = runSetup("--agent", "claude", "--mcp")
		require.ErrorIs(t, err, agentsetup.ErrNotReady)
		assert.Contains(t, readFile(t, filepath.Join(home, "settings.json")), missing)
		root := newRootCmd()
		var out, stderr bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&stderr)
		assert.Equal(t, 1, executeCLI(root, []string{"agent", "setup", "--agent", "claude"}))
		assert.Empty(t, stderr.String())
		registration, found, err := mcpconfig.Registration(mcpconfig.Options{Agent: skills.AgentClaude})
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, missing, registration.Command)
	})
}

func TestAgentSetupRepairsMCPExecutableWhenHookMoves(t *testing.T) {
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
	assert.Contains(t, readFile(t, filepath.Join(home, "settings.json")), exe)
	registration, found, err := mcpconfig.Registration(mcpconfig.Options{Agent: skills.AgentClaude})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, exe, registration.Command)
	assert.Equal(t, []string{"--server", "http://127.0.0.1:8181", "mcp", "serve"}, registration.Args)
	assert.Equal(t, map[string]string{"TOKEN": "example"}, registration.Env)
	out, err = runSetup("--agent", "claude", "--mcp")
	require.NoError(t, err, out)
	got, _, err := mcpconfig.Registration(mcpconfig.Options{Agent: skills.AgentClaude})
	require.NoError(t, err)
	assert.Equal(t, registration, got)
}

func TestQuickstartCodexTrust(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	fakeAgent(t, "codex")
	t.Setenv("ROBOREV_TEST_HOOK_TRUST", "trusted")
	_, err := runSetup("--agent", "codex", "--mcp")
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "mcp-probe")
	t.Setenv("ROBOREV_TEST_MCP_MARKER", marker)
	check := checkNativeAgentHook(t.Context(), "agent_hook_codex", "codex")
	assert.Equal(t, statusOK, check.Status)
	assert.NotContains(t, check.Details, "mcp roborev")
	t.Setenv("ROBOREV_TEST_HOOK_TRUST", "untrusted")
	check = checkNativeAgentHook(t.Context(), "agent_hook_codex", "codex")
	assert.Equal(t, checkStatus("untrusted"), check.Status)
	assert.Contains(t, check.FixCommand, "/hooks")
	t.Setenv("ROBOREV_TEST_HOOK_TRUST", "trusted")
	require.NoError(t, os.RemoveAll(filepath.Join(os.Getenv("CODEX_HOME"), "skills")))
	check = checkNativeAgentHook(t.Context(), "agent_hook_codex", "codex")
	assert.Equal(t, statusOK, check.Status)
	_, err = os.Stat(marker)
	assert.True(t, os.IsNotExist(err))
}

func TestDoctorReportsDaemonStartedDuringMCPCheck(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	fakeAgent(t, "codex")
	t.Setenv("ROBOREV_TEST_HOOK_TRUST", "trusted")
	_, err := runSetup("--agent", "codex", "--mcp")
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "mcp-probe")
	t.Setenv("ROBOREV_TEST_MCP_MARKER", marker)
	env := loadDoctorEnv(t.Context(), "", fakeDoctorDaemon{pingFn: func() (*daemon.PingInfo, error) {
		if _, err := os.Stat(marker); err != nil {
			return nil, errors.New("connection refused")
		}
		return &daemon.PingInfo{PID: 123}, nil
	}})
	require.NotEmpty(t, env.agentReach)
	assert.Equal(t, "agents.reach.codex", env.agentReach[0].ID)
	check := findDoctorCheck(t, checkDoctorDaemon(env), "daemon.running")
	assert.Equal(t, doctorWarn, check.Status)
	assert.Contains(t, check.Summary, "before")
	assert.Contains(t, check.Summary, "now running")
	assert.Contains(t, check.Summary, "123")
}

func TestSetupIgnoresMalformedUnselectedMCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	path := filepath.Join(home, ".claude.json")
	const malformed = `{"mcpServers":{"roborev":{"command":"roborev","args":"invalid"}}}`
	require.NoError(t, os.WriteFile(path, []byte(malformed), 0o600))
	out, err := runSetup("--agent", "claude")
	require.NoError(t, err, out)
	out, err = runSetup("--agent", "claude", "--check")
	require.NoError(t, err, out)
	assert.Equal(t, malformed, readFile(t, path))
}

func TestReadinessUsesInstalledHookModeAndSuggestsMCPRepair(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	exe, err := os.Executable()
	require.NoError(t, err)
	_, err = mcpconfig.Install(mcpconfig.Options{Agent: skills.AgentClaude, Executable: exe})
	require.NoError(t, err)
	require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Executable: exe}, io.Discard))
	_, details := agentReadiness(t.Context(), "claude", false)
	require.Len(t, details, 2)
	assert.Contains(t, details[0], "hooks:")
	assert.Contains(t, details[1], "skills:")
	assert.NotContains(t, strings.Join(details, " "), "mcp roborev")
	_, err = runSetup("--agent", "claude", "--mcp")
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(home, ".claude.json")))
	row, _ := agentReadiness(t.Context(), "claude", false)
	assert.Equal(t, agentsetup.StateMissing, row.State)
	assert.NotEmpty(t, row.NextStep)
}

func TestAgentSetupPartialMCPInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	require.NoError(t, os.WriteFile(filepath.Join(home, "skills"), []byte("blocks skills"), 0o600))
	_, err := runSetup("--agent", "claude", "--mcp", "--server", "http://127.0.0.1:8282")
	require.Error(t, err)
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
			if path := os.Getenv("ROBOREV_TEST_MCP_MARKER"); path != "" {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					return 4
				}
			}
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
	for _, tc := range []struct {
		name       string
		mcp        bool
		args       []string
		skillState agentsetup.State
	}{
		{"existing MCP", true, []string{"--server", "http://127.0.0.1:8282"}, agentsetup.StateOK},
		{"switch to MCP", false, []string{"--mcp"}, agentsetup.StateUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", home)
			fakeAgent(t, "claude")
			exe, err := os.Executable()
			require.NoError(t, err)
			require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Executable: exe, MCP: tc.mcp}, io.Discard))
			path := filepath.Join(home, "settings.json")
			var doc map[string]any
			require.NoError(t, json.Unmarshal([]byte(readFile(t, path)), &doc))
			handler := doc["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
			handler["command"] = handler["command"].(string) + " --turn-threshold 7"
			handler["timeout"] = float64(23)
			data, err := json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o600))
			skillPath := filepath.Join(home, "skills", "roborev-fix", "SKILL.md")
			skill := readFile(t, skillPath)
			mcpPath := filepath.Join(home, ".claude.json")
			var mcp string
			if tc.mcp {
				mcp = readFile(t, mcpPath)
			}
			states, err := setupReport(t, append([]string{"--agent", "claude"}, tc.args...)...)
			require.ErrorIs(t, err, agentsetup.ErrNotReady)
			assert.Equal(t, agentsetup.StateUnknown, states["hooks"])
			assert.Equal(t, agentsetup.StateUnknown, states["mcp roborev"])
			assert.Equal(t, tc.skillState, states["skills"])
			assert.Equal(t, string(data), readFile(t, path))
			assert.Equal(t, skill, readFile(t, skillPath))
			if tc.mcp {
				assert.Equal(t, mcp, readFile(t, mcpPath))
			} else {
				_, err = os.Stat(mcpPath)
				assert.True(t, os.IsNotExist(err))
			}
		})
	}
}

func TestSetupMCPInstallFailureKeepsCLIMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions differ on Windows")
	}
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	out, err := runSetup("--agent", "claude")
	require.NoError(t, err, out)
	hookPath := filepath.Join(home, "settings.json")
	skillPath := filepath.Join(home, "skills", "roborev-fix", "SKILL.md")
	hooks, skill := readFile(t, hookPath), readFile(t, skillPath)
	require.NoError(t, os.Chmod(home, 0o500))
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	out, err = runSetup("--agent", "claude", "--mcp")
	require.ErrorIs(t, err, agentsetup.ErrNotReady, out)
	assert.Equal(t, hooks, readFile(t, hookPath))
	assert.Equal(t, skill, readFile(t, skillPath))
}

func TestSetupHookInstallFailureKeepsMCPSkills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions differ on Windows")
	}
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	out, err := runSetup("--agent", "claude", "--mcp")
	require.NoError(t, err, out)
	hookPath := filepath.Join(home, "settings.json")
	skillPath := filepath.Join(home, "skills", "roborev-fix", "SKILL.md")
	hooks, skill := readFile(t, hookPath), readFile(t, skillPath)
	require.NoError(t, os.Chmod(home, 0o500))
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	out, err = runSetup("--agent", "claude", "--mcp=false")
	require.ErrorIs(t, err, agentsetup.ErrNotReady, out)
	assert.Equal(t, hooks, readFile(t, hookPath))
	assert.Equal(t, skill, readFile(t, skillPath))
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
	assert.Contains(t, out, `"state":"failed"`)
	assert.Contains(t, readFile(t, filepath.Join(codex, "hooks.json")), "roborev-agent-hook")
	assert.Equal(t, "{", readFile(t, filepath.Join(claude, "settings.json")))
}
