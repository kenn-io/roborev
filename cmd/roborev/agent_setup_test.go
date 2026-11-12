package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	"go.kenn.io/roborev/internal/mcpserver"
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

func TestSetupReportsWrongMCPTools(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fakeAgent(t, "claude")
	_, err := runSetup("--agent", "claude", "--mcp")
	require.NoError(t, err)
	t.Setenv("ROBOREV_TEST_WRONG_MCP", "1")
	states, err := setupReport(t, "--agent", "claude", "--check")
	require.Error(t, err)
	assert.Equal(t, agentsetup.StateUnknown, states["mcp roborev"])
}

func TestProbeMCPToolsUsesSelectedHTTPSettings(t *testing.T) {
	t.Setenv("ROBOREV_TEST_MCP_TOKEN", "secret")
	backend := mcpserver.NewHTTPBackend("http://127.0.0.1:1", http.DefaultClient)
	handler := mcpserver.New(backend, "test", nil).HTTPHandler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("X-Example") != "selected" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	err := probeMCPTools(t.Context(), mcpconfig.RegisteredServer{URL: server.URL + "/mcp", Headers: map[string]string{"X-Example": "selected"}, BearerTokenEnvVar: "ROBOREV_TEST_MCP_TOKEN"})
	require.NoError(t, err)
}

func TestSetupReportsMatcherWithoutBash(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	exe, err := os.Executable()
	require.NoError(t, err)
	require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Executable: exe}, io.Discard))
	path := filepath.Join(home, "settings.json")
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(readFile(t, path)), &doc))
	hooks := doc["hooks"].(map[string]any)
	hooks["PreToolUse"].([]any)[0].(map[string]any)["matcher"] = "Write"
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	states, err := setupReport(t, "--agent", "claude", "--check")
	require.Error(t, err)
	assert.Equal(t, agentsetup.StateUnknown, states["hooks"])
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
	assert.Equal(t, agentsetup.StateOK, status.State)
	assert.Equal(t, string(data), readFile(t, result.Path))

	item, err = agentMCPItem("claude", exe, "http://127.0.0.1:8282", server, true)
	require.NoError(t, err)
	status, err = item.Check(t.Context())
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateOutdated, status.State)
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
	require.ErrorIs(t, err, agentsetup.ErrNotReady, out)
	exe, err := os.Executable()
	require.NoError(t, err)
	assert.Contains(t, readFile(t, filepath.Join(home, "settings.json")), exe)
	registration, found, err := mcpconfig.Registration(mcpconfig.Options{Agent: skills.AgentClaude})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, missing, registration.Command)
	assert.Equal(t, []string{"--server", "http://127.0.0.1:8181", "mcp", "serve"}, registration.Args)
	assert.Equal(t, map[string]string{"TOKEN": "example"}, registration.Env)
}

func TestQuickstartCodexTrust(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	fakeAgent(t, "codex")
	t.Setenv("ROBOREV_TEST_HOOK_TRUST", "trusted")
	_, err := runSetup("--agent", "codex", "--mcp")
	require.NoError(t, err)
	check := checkNativeAgentHook(t.Context(), "agent_hook_codex", "codex")
	assert.Equal(t, statusOK, check.Status)
	assert.Contains(t, check.Details, "mcp roborev: ok")
	t.Setenv("ROBOREV_TEST_HOOK_TRUST", "untrusted")
	check = checkNativeAgentHook(t.Context(), "agent_hook_codex", "codex")
	assert.Equal(t, checkStatus("untrusted"), check.Status)
	assert.Contains(t, check.FixCommand, "/hooks")
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
	_, details := agentReadiness(t.Context(), "claude")
	require.Len(t, details, 2)
	assert.Contains(t, details[0], "hooks:")
	assert.Contains(t, details[1], "skills:")
	assert.NotContains(t, strings.Join(details, " "), "mcp roborev")
	_, err = runSetup("--agent", "claude", "--mcp")
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(home, ".claude.json")))
	row, _ := agentReadiness(t.Context(), "claude")
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
	assert.Contains(t, out, `"state":"failed"`)
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
	assert.InDelta(t, 23, group["hooks"].([]any)[0].(map[string]any)["timeout"], 0.001)
	assert.Contains(t, group["hooks"].([]any)[0].(map[string]any)["command"], "http://127.0.0.1:8282")
	require.Contains(t, hooks, "Stop")
	stop := hooks["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	assert.Contains(t, stop["command"], "http://127.0.0.1:8282")
}
