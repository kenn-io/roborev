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
	"go.kenn.io/kit/agentsetup"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/mcpconfig"
	"go.kenn.io/roborev/internal/skills"
)

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

func fakeAgent(t *testing.T, name string) {
	t.Helper()
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

func setupReport(t *testing.T, args ...string) (map[string]agentsetup.State, error) {
	t.Helper()
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
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func TestAgentSetupMCPReruns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	fakeAgent(t, "codex")
	t.Setenv("ROBOREV_TEST_HOOK_TRUST", "trusted")
	out, err := runSetup("--agent", "codex", "--mcp")
	require.NoError(t, err, out)
	path := filepath.Join(home, "config.toml")
	hooks := readFile(t, filepath.Join(home, "hooks.json"))
	config := readFile(t, path)
	out, err = runSetup("--agent", "codex", "--check")
	require.NoError(t, err, out)
	assert.Equal(t, hooks, readFile(t, filepath.Join(home, "hooks.json")))
	assert.Equal(t, config, readFile(t, path))
	states, err := setupReport(t, "--agent", "codex", "--check")
	require.NoError(t, err)
	assert.Equal(t, agentsetup.StateOK, states["mcp roborev"])
}

func TestAgentSetupPreservesMCPSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	_, err := mcpconfig.Install(mcpconfig.Options{Agent: skills.AgentClaude})
	require.NoError(t, err)
	path := filepath.Join(home, ".claude.json")
	var config map[string]any
	require.NoError(t, json.Unmarshal([]byte(readFile(t, path)), &config))
	server := config["mcpServers"].(map[string]any)["roborev"].(map[string]any)
	server["env"] = map[string]any{"TOKEN": "example"}
	server["headers"] = map[string]any{"X-Client": "roborev"}
	data, err := json.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	out, err := runSetup("--agent", "claude", "--mcp")
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(readFile(t, path)), &config))
	server = config["mcpServers"].(map[string]any)["roborev"].(map[string]any)
	assert.Equal(t, map[string]any{"TOKEN": "example"}, server["env"])
	assert.Equal(t, map[string]any{"X-Client": "roborev"}, server["headers"])
}

func TestAgentSetupKeepsResolvableHookExecutable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	exe, err := os.Executable()
	require.NoError(t, err)
	data, err := os.ReadFile(exe)
	require.NoError(t, err)
	moved := filepath.Join(t.TempDir(), "roborev-moved")
	if runtime.GOOS == "windows" {
		moved += ".exe"
	}
	require.NoError(t, os.WriteFile(moved, data, 0o700))
	require.NoError(t, agenthook.RunInstall(agenthook.InstallOptions{Agent: "claude", Executable: moved, Timeout: 10 * time.Second}, io.Discard))
	row, _ := agentReadiness(t.Context(), "claude", true)
	assert.Equal(t, agentsetup.StateOK, row.State, row.Reason)
	out, err := runSetup("--agent", "claude")
	require.NoError(t, err, out)
	assert.Contains(t, readFile(t, filepath.Join(home, "settings.json")), moved)
}

func TestAgentSetupReportsUnsupportedAgentUnknown(t *testing.T) {
	states, err := setupReport(t, "--agent", "gemini")
	require.ErrorIs(t, err, agentsetup.ErrNotReady)
	assert.Equal(t, agentsetup.StateUnknown, states["setup"])
}

func TestAgentSetupDoesNotInstallHooksWhenMCPFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	fakeAgent(t, "claude")
	path := filepath.Join(home, ".claude.json")
	require.NoError(t, os.Mkdir(path, 0o700))
	_, err := runSetup("--agent", "claude", "--mcp")
	require.ErrorIs(t, err, agentsetup.ErrNotReady)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	_, err = os.Stat(filepath.Join(home, "settings.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(home, "skills"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestDoctorReportsDaemonStartedDuringMCPCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
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

func TestAgentReadinessShowsNextStepOnce(t *testing.T) {
	rows := []agentsetup.Row{
		{Piece: "hooks", State: agentsetup.StateUntrusted, NextStep: "Open Codex and run /hooks"},
		{Piece: "skills", State: agentsetup.StateOK},
	}
	worst, details := readinessSummary(rows)
	assert.Equal(t, "Open Codex and run /hooks", worst.NextStep)
	assert.Equal(t, []string{"hooks: untrusted", "skills: ok"}, details)
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
