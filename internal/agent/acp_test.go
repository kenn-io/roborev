package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestACPAgent(t *testing.T) {
	acpAgent := NewACPAgent("test-acp-agent")

	assert.Equal(t, "acp", acpAgent.Name())

	assert.Equal(t, "test-acp-agent", acpAgent.CommandName())

	thoroughAgent := acpAgent.WithReasoning(ReasoningThorough)
	assert.Equal(t, ReasoningThorough, thoroughAgent.(*ACPAgent).Reasoning)

	agenticAgent := acpAgent.WithAgentic(true)
	assert.True(t, agenticAgent.(*ACPAgent).Agentic)

	modelAgent := acpAgent.WithModel("gpt-4")
	assert.Equal(t, "gpt-4", modelAgent.(*ACPAgent).Model)

	assert.True(t, acpAgent.WithAgentic(true).WithModel("gpt-4").(*ACPAgent).Agentic)

	defaultAgent := NewACPAgent("")
	assert.Equal(t, "acp-agent", defaultAgent.Command)
	assert.Equal(t, "plan", defaultAgent.Mode)
	assert.Equal(t, 10*time.Minute, defaultAgent.Timeout)

	configuredAgent := NewACPAgentFromConfig(&config.ACPAgentConfig{
		Name:            "custom-acp",
		Command:         "custom-command",
		ReadOnlyMode:    "plan",
		AutoApproveMode: "auto-approve",
		Mode:            "plan",
	})
	assert.Equal(t, "custom-acp", configuredAgent.Name())
}

func TestACPAgentCommandLine(t *testing.T) {
	agent := NewACPAgent("acp-agent")

	cmdLine := agent.CommandLine()
	assert.Contains(t, cmdLine, "acp-agent")

	withModel := agent.WithModel("claude-3-opus")
	assert.Equal(t, withModel.CommandLine(), agent.CommandLine())

	agentic := agent.WithAgentic(true)
	assert.Equal(t, agent.CommandLine(), agentic.CommandLine())
}

func TestApplyACPAgentConfigOverrideModeResolution(t *testing.T) {
	tests := []struct {
		name               string
		override           *config.ACPAgentConfig
		wantReadOnly       string
		wantMode           string
		wantDisableModeNeg bool
	}{
		{
			name: "read_only_mode only",
			override: &config.ACPAgentConfig{
				ReadOnlyMode: "safe-plan",
			},
			wantReadOnly:       "safe-plan",
			wantMode:           "safe-plan",
			wantDisableModeNeg: false,
		},
		{
			name: "mode only",
			override: &config.ACPAgentConfig{
				Mode: "custom-mode",
			},
			wantReadOnly:       defaultACPReadOnlyMode,
			wantMode:           "custom-mode",
			wantDisableModeNeg: false,
		},
		{
			name: "both mode and read_only_mode",
			override: &config.ACPAgentConfig{
				ReadOnlyMode: "safe-plan",
				Mode:         "agentic-mode",
			},
			wantReadOnly:       "safe-plan",
			wantMode:           "agentic-mode",
			wantDisableModeNeg: false,
		},
		{
			name: "disable mode negotiation",
			override: &config.ACPAgentConfig{
				DisableModeNegotiation: true,
			},
			wantReadOnly:       defaultACPReadOnlyMode,
			wantMode:           "",
			wantDisableModeNeg: true,
		},
		{
			name: "neither mode nor read_only_mode",
			override: &config.ACPAgentConfig{
				Model: "devstral-2",
			},
			wantReadOnly:       defaultACPReadOnlyMode,
			wantMode:           defaultACPReadOnlyMode,
			wantDisableModeNeg: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultACPAgentConfig()
			applyACPAgentConfigOverride(cfg, tc.override)

			require.Equal(t, tc.wantReadOnly, cfg.ReadOnlyMode, "ReadOnlyMode = %q, want %q", cfg.ReadOnlyMode, tc.wantReadOnly)
			require.Equal(t, tc.wantMode, cfg.Mode, "Mode = %q, want %q", cfg.Mode, tc.wantMode)
			require.Equal(t, tc.wantDisableModeNeg, cfg.DisableModeNegotiation, "DisableModeNegotiation = %v, want %v", cfg.DisableModeNegotiation, tc.wantDisableModeNeg)
		})
	}
}

func TestNewACPAgentFromConfigDisableModeNegotiation(t *testing.T) {
	t.Parallel()

	agent := NewACPAgentFromConfig(&config.ACPAgentConfig{
		Command:                "go",
		Mode:                   "plan",
		ReadOnlyMode:           "plan",
		AutoApproveMode:        "auto-approve",
		DisableModeNegotiation: true,
	})
	require.Empty(t, agent.Mode, "expected mode negotiation disabled (empty mode), got %q", agent.Mode)

	agentic := agent.WithAgentic(true).(*ACPAgent)
	require.Empty(t, agentic.Mode, "expected WithAgentic(true) to preserve disabled mode negotiation, got %q", agentic.Mode)
	require.True(t, agentic.mutatingOperationsAllowed(), "expected mutating operations allowed in agentic mode when negotiation is disabled")

	nonAgentic := agent.WithAgentic(false).(*ACPAgent)
	require.Empty(t, nonAgentic.Mode, "expected WithAgentic(false) to preserve disabled mode negotiation, got %q", nonAgentic.Mode)
	require.False(t, nonAgentic.mutatingOperationsAllowed(), "expected mutating operations denied in non-agentic mode when negotiation is disabled")
}

func TestGetAvailableWithConfigResolvesACPAlias(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ACP: &config.ACPAgentConfig{
			Name:    "custom-acp",
			Command: "go",
		},
	}

	resolved, err := GetAvailableWithConfig("", "custom-acp", cfg)
	require.NoError(t, err, "GetAvailableWithConfig failed: %v")

	acpAgent, ok := resolved.(*ACPAgent)
	require.True(t, ok, "Expected ACP agent, got %T", resolved)
	require.Equal(t, "acp", acpAgent.Name(), "Expected canonical ACP name 'acp', got %q", acpAgent.Name())
	require.Equal(t, "go", acpAgent.Command, "Expected ACP command from config, got %q", acpAgent.Command)
}

func TestGetAvailableWithConfigEmptyRepoPathDoesNotReadCWD(t *testing.T) {
	cwd := t.TempDir()
	err := os.WriteFile(
		filepath.Join(cwd, ".roborev.toml"),
		[]byte("[acp]\nname = \"cwd-acp\"\ncommand = \"cwd-acp\"\n"),
		0o644,
	)
	require.NoError(t, err)
	t.Chdir(cwd)

	fakeBin := t.TempDir()
	globalACPName := "global-acp"
	if runtime.GOOS == "windows" {
		globalACPName += ".exe"
	}
	globalACP := filepath.Join(fakeBin, globalACPName)
	require.NoError(t, os.WriteFile(globalACP, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", fakeBin)

	cfg := &config.Config{
		ACP: &config.ACPAgentConfig{
			Name:    "global-acp",
			Command: "global-acp",
		},
	}

	resolved, err := GetAvailableWithConfig("", "global-acp", cfg)
	require.NoError(t, err)

	acpAgent, ok := resolved.(*ACPAgent)
	require.True(t, ok, "Expected ACP agent, got %T", resolved)
	require.Equal(t, "acp", acpAgent.Name())
	require.Equal(t, "global-acp", acpAgent.Command)
}

func TestGetAvailableWithConfigResolvesConfiguredACPNameAlias(t *testing.T) {
	fakeBin := t.TempDir()
	binName := defaultACPCommand
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	acpPath := filepath.Join(fakeBin, binName)
	if err := os.WriteFile(acpPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		require.NoError(t, err, "failed to create fake acp-agent binary: %v")
	}
	t.Setenv("PATH", fakeBin)

	cfg := &config.Config{
		ACP: &config.ACPAgentConfig{
			Name:    "claude",
			Command: defaultACPCommand,
		},
	}

	resolved, err := GetAvailableWithConfig("", "claude", cfg)
	require.NoError(t, err, "GetAvailableWithConfig failed: %v")

	acpAgent, ok := resolved.(*ACPAgent)
	require.True(t, ok, "Expected ACP agent, got %T", resolved)
	require.Equal(t, "acp", acpAgent.Name(), "Expected canonical ACP name 'acp', got %q", acpAgent.Name())
	require.Equal(t, defaultACPCommand, acpAgent.Command, "Expected ACP command %q, got %q", defaultACPCommand, acpAgent.Command)
}

func TestGetAvailableWithConfigFallsBackToCanonicalACPWhenConfiguredCommandMissing(t *testing.T) {
	fakeBin := t.TempDir()
	binName := "acp-agent"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	acpPath := filepath.Join(fakeBin, binName)
	if err := os.WriteFile(acpPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		require.NoError(t, err, "failed to create fake acp-agent binary: %v")
	}
	t.Setenv("PATH", fakeBin)

	cfg := &config.Config{
		ACP: &config.ACPAgentConfig{
			Name:    "custom-acp",
			Command: "missing-acp-command",
		},
	}

	resolved, err := GetAvailableWithConfig("", "custom-acp", cfg)
	require.NoError(t, err, "GetAvailableWithConfig failed: %v")

	commandAgent, ok := resolved.(CommandAgent)
	require.True(t, ok, "Expected resolved agent to implement CommandAgent, got %T", resolved)
	require.Equal(t, defaultACPCommand, commandAgent.CommandName(), "Expected fallback to canonical command %q, got %q", defaultACPCommand, commandAgent.CommandName())
}

func TestGetAvailableWithConfigResolvedACPBranchFallsBackWhenConfiguredCommandMissing(t *testing.T) {
	originalRegistry := registry
	registry = map[string]Agent{
		defaultACPName: NewACPAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	fakeBin := t.TempDir()
	binName := defaultACPCommand
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	acpPath := filepath.Join(fakeBin, binName)
	if err := os.WriteFile(acpPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		require.NoError(t, err, "failed to create fake acp-agent binary: %v")
	}
	t.Setenv("PATH", fakeBin)

	cfg := &config.Config{
		ACP: &config.ACPAgentConfig{
			Name:    "custom-acp",
			Command: "missing-acp-command",
		},
	}

	resolved, err := GetAvailableWithConfig("", "custom-acp", cfg)
	require.NoError(t, err, "GetAvailableWithConfig failed: %v")

	commandAgent, ok := resolved.(CommandAgent)
	require.True(t, ok, "Expected resolved agent to implement CommandAgent, got %T", resolved)
	require.Equal(t, defaultACPCommand, commandAgent.CommandName(), "Expected fallback to canonical command %q, got %q", defaultACPCommand, commandAgent.CommandName())
}

func terminalCount(client *acpClient) int {
	client.terminalsMutex.Lock()
	defer client.terminalsMutex.Unlock()
	return len(client.terminals)
}

func terminalExists(client *acpClient, terminalID string) bool {
	_, exists := client.getTerminal(terminalID)
	return exists
}

func TestACPAgentTerminalFunctionality(t *testing.T) {
	agent := &ACPAgent{
		SessionID:       "test-session",
		ReadOnlyMode:    "read-only",
		AutoApproveMode: "auto-approve",
		Mode:            "auto-approve",
	}
	client := &acpClient{
		agent:          agent,
		terminals:      make(map[string]*acpTerminal),
		nextTerminalID: 1,
	}

	t.Run("Terminal creation and storage", func(t *testing.T) {
		cmd, args := acpTestEchoCommand("test")
		resp, err := client.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
			SessionId: "test-session",
			Command:   cmd,
			Args:      args,
		})
		require.NoError(t, err, "Failed to create terminal: %v")

		assert.Equal(t, "term-1", resp.TerminalId)

		assert.Equal(t, 1, terminalCount(client))

		_, err = client.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{
			SessionId:  "test-session",
			TerminalId: resp.TerminalId,
		})
		require.NoError(t, err)
	})

	t.Run("Output truncation", func(t *testing.T) {
		cmd, args := acpTestEchoCommand("test output")
		resp, err := client.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
			SessionId:       "test-session",
			Command:         cmd,
			Args:            args,
			OutputByteLimit: new(5),
		})
		require.NoError(t, err, "Failed to create terminal: %v")

		waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := client.WaitForTerminalExit(waitCtx, acp.WaitForTerminalExitRequest{
			SessionId:  "test-session",
			TerminalId: resp.TerminalId,
		}); err != nil {
			require.NoError(t, err, "WaitForTerminalExit failed: %v")
		}

		outputResp, err := client.TerminalOutput(context.Background(), acp.TerminalOutputRequest{
			SessionId:  "test-session",
			TerminalId: resp.TerminalId,
		})
		require.NoError(t, err)
		assert.LessOrEqual(t, len(outputResp.Output), 5)
		assert.True(t, outputResp.Truncated, "Expected output to be marked truncated when output exceeds byte limit")

		_, err = client.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{
			SessionId:  "test-session",
			TerminalId: resp.TerminalId,
		})
		require.NoError(t, err)
	})

	t.Run("Terminal release with context cancellation", func(t *testing.T) {
		cmd, args := acpTestEchoCommand("test")
		resp, err := client.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
			SessionId: "test-session",
			Command:   cmd,
			Args:      args,
		})
		require.NoError(t, err, "Failed to create terminal: %v")

		_, err = client.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{
			SessionId:  "test-session",
			TerminalId: resp.TerminalId,
		})
		require.NoError(t, err)

		assert.Equal(t, 0, terminalCount(client))
	})

	t.Run("Session ID validation", func(t *testing.T) {
		cmd, args := acpTestEchoCommand("test")
		resp, err := client.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
			SessionId: "test-session",
			Command:   cmd,
			Args:      args,
		})
		require.NoError(t, err, "Failed to create terminal: %v")

		_, err = client.TerminalOutput(context.Background(), acp.TerminalOutputRequest{
			SessionId:  "wrong-session",
			TerminalId: resp.TerminalId,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "session ID mismatch", "unexpected error")

		_, err = client.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{
			SessionId:  "test-session",
			TerminalId: resp.TerminalId,
		})
		require.NoError(t, err)
	})

	t.Run("Terminal lifecycle - persists after command completion", func(t *testing.T) {
		cmd, args := acpTestEchoCommand("test")
		resp, err := client.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
			SessionId: "test-session",
			Command:   cmd,
			Args:      args,
		})
		require.NoError(t, err, "Failed to create terminal: %v")

		assert.Equal(t, 1, terminalCount(client))

		waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		waitResp, err := client.WaitForTerminalExit(waitCtx, acp.WaitForTerminalExitRequest{
			SessionId:  "test-session",
			TerminalId: resp.TerminalId,
		})
		require.NoError(t, err, "WaitForTerminalExit failed: %v")

		require.Equal(t, new(0), waitResp.ExitCode, "Expected exit code 0, got %+v", waitResp)

		require.True(t, terminalExists(client, resp.TerminalId), "Expected terminal %s to persist after completion", resp.TerminalId)

		outputResp, err := client.TerminalOutput(context.Background(), acp.TerminalOutputRequest{
			SessionId:  "test-session",
			TerminalId: resp.TerminalId,
		})
		require.NoError(t, err, "TerminalOutput failed: %v")

		require.Equal(t, new(0), outputResp.ExitStatus.ExitCode, "Expected terminal output to include exit status 0, got %+v", outputResp.ExitStatus)

		_, err = client.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{
			SessionId:  "test-session",
			TerminalId: resp.TerminalId,
		})
		require.NoError(t, err)
	})

	t.Run("CreateTerminal defaults cwd to repo root when omitted", func(t *testing.T) {
		tempDir := t.TempDir()
		client.agent.repoRoot = tempDir
		client.repoRoot = tempDir

		cmd, args := acpTestEchoCommand("cwd")
		resp, err := client.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
			SessionId: "test-session",
			Command:   cmd,
			Args:      args,
		})
		require.NoError(t, err, "Failed to create terminal: %v")

		term, exists := client.getTerminal(resp.TerminalId)
		require.True(t, exists, "terminal %s not found", resp.TerminalId)
		resolvedTempDir, err := filepath.EvalSymlinks(tempDir)
		require.NoError(t, err, "Failed to resolve temp dir: %v")

		require.True(t, pathWithinRoot(term.cmd.Dir, resolvedTempDir), "Expected terminal cwd %q to be within %q", term.cmd.Dir, resolvedTempDir)

		_, err = client.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{
			SessionId:  "test-session",
			TerminalId: resp.TerminalId,
		})
		require.NoError(t, err)
	})

	t.Run("CreateTerminal resolves relative cwd against repo root", func(t *testing.T) {
		tempDir := t.TempDir()
		subdir := filepath.Join(tempDir, "sub")
		if err := os.MkdirAll(subdir, 0o755); err != nil {
			require.NoError(t, err, "Failed to create subdir: %v")
		}
		client.agent.repoRoot = tempDir
		client.repoRoot = tempDir

		relative := "sub"
		cmd, args := acpTestEchoCommand("cwd")
		resp, err := client.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
			SessionId: "test-session",
			Command:   cmd,
			Args:      args,
			Cwd:       &relative,
		})
		require.NoError(t, err, "Failed to create terminal: %v")

		term, exists := client.getTerminal(resp.TerminalId)
		require.True(t, exists, "terminal %s not found", resp.TerminalId)
		resolvedSubdir, err := filepath.EvalSymlinks(subdir)
		require.NoError(t, err, "Failed to resolve subdir: %v")

		require.Equal(t, resolvedSubdir, term.cmd.Dir, "Expected terminal cwd %q, got %q", resolvedSubdir, term.cmd.Dir)

		_, err = client.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{
			SessionId:  "test-session",
			TerminalId: resp.TerminalId,
		})
		require.NoError(t, err)
	})

	t.Run("CreateTerminal rejects cwd traversal outside repo root", func(t *testing.T) {
		tempDir := t.TempDir()
		client.agent.repoRoot = tempDir
		client.repoRoot = tempDir

		outsideDir := filepath.Join(filepath.Dir(tempDir), "outside")
		if err := os.MkdirAll(outsideDir, 0o755); err != nil {
			require.NoError(t, err, "Failed to create outside dir: %v")
		}

		relative := "../outside"
		cmd, args := acpTestEchoCommand("cwd")
		_, err := client.CreateTerminal(context.Background(), acp.CreateTerminalRequest{
			SessionId: "test-session",
			Command:   cmd,
			Args:      args,
			Cwd:       &relative,
		})
		require.Error(t, err, "Expected error for cwd traversal outside repo root")

		if !strings.Contains(err.Error(), "outside repository root") {
			require.NoError(t, err, "Expected outside repository root error, got: %v")
		}
	})

	t.Run("CreateTerminal lifetime outlives request context", func(t *testing.T) {
		cmd, args, ok := acpTestSleepCommand(1)
		if !ok {
			t.Skip("sleep command not available")
		}
		client.agent.repoRoot = ""
		client.repoRoot = ""

		reqCtx, reqCancel := context.WithCancel(context.Background())
		resp, err := client.CreateTerminal(reqCtx, acp.CreateTerminalRequest{
			SessionId: "test-session",
			Command:   cmd,
			Args:      args,
		})
		require.NoError(t, err, "Failed to create terminal: %v")

		reqCancel()

		term, exists := client.getTerminal(resp.TerminalId)
		require.True(t, exists, "terminal %s not found", resp.TerminalId)
		select {
		case <-term.context.Done():
			require.Condition(t, func() bool { return false }, "terminal context canceled by request context cancellation")
		case <-time.After(50 * time.Millisecond):

		}

		_, err = client.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{
			SessionId:  "test-session",
			TerminalId: resp.TerminalId,
		})
		require.NoError(t, err)
	})

	t.Run("WaitForTerminalExit does not block other terminal operations", func(t *testing.T) {
		blockedDone := make(chan struct{})
		blockedTerminal := &acpTerminal{
			id:   "blocked",
			done: blockedDone,
		}
		client.addTerminal(blockedTerminal)

		waitDone := make(chan struct{})
		waitErr := make(chan error, 1)
		waitResp := make(chan acp.WaitForTerminalExitResponse, 1)
		go func() {
			resp, err := client.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{
				SessionId:  "test-session",
				TerminalId: "blocked",
			})
			if err != nil {
				waitErr <- err
				close(waitDone)
				return
			}
			waitResp <- resp
			close(waitDone)
		}()

		addDone := make(chan struct{})
		go func() {
			client.addTerminal(&acpTerminal{
				id:   "secondary",
				done: make(chan struct{}),
			})
			close(addDone)
		}()

		select {
		case <-addDone:

		case <-time.After(200 * time.Millisecond):
			require.Condition(t, func() bool { return false }, "addTerminal blocked while WaitForTerminalExit was waiting")
		}

		blockedTerminal.setExitStatus(&acp.TerminalExitStatus{ExitCode: new(0)})
		close(blockedDone)

		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			require.Condition(t, func() bool { return false }, "WaitForTerminalExit did not return after done channel close")
		}

		select {
		case err := <-waitErr:
			require.NoError(t, err, "WaitForTerminalExit returned error: %v")
		default:
		}

		select {
		case resp := <-waitResp:
			require.Equal(t, new(0), resp.ExitCode, "Expected exit code 0, got %+v", resp)
		default:
			require.Condition(t, func() bool { return false }, "missing WaitForTerminalExit response")
		}

		client.removeTerminal("blocked")
		client.removeTerminal("secondary")
	})
}

func TestACPNoDoubleMutexUnlockPanics(t *testing.T) {
	agent := &ACPAgent{
		SessionID:       "test-session",
		ReadOnlyMode:    "read-only",
		AutoApproveMode: "auto-approve",
		Mode:            "auto-approve",
	}
	client := &acpClient{
		agent:          agent,
		terminals:      make(map[string]*acpTerminal),
		nextTerminalID: 1,
	}

	_, err := client.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{
		TerminalId: "non-existent-terminal",
	})
	if err != nil {
		t.Logf("Expected error for non-existent terminal: %v", err)
	}
}

func TestBoundedWriter(t *testing.T) {
	t.Run("Write within limit", func(t *testing.T) {
		buf := &bytes.Buffer{}
		mutex := &sync.Mutex{}
		writer := &boundedWriter{
			writer: &threadSafeWriter{
				buf:   buf,
				mutex: mutex,
			},
			maxSize:   10,
			truncated: false,
		}

		n, err := writer.Write([]byte("hello"))
		require.NoError(t, err, "Write failed: %v")

		assert.Equal(t, 5, n)
		assert.Equal(t, "hello", buf.String())
		assert.False(t, writer.truncated)
	})

	t.Run("Write exactly at limit", func(t *testing.T) {
		buf := &bytes.Buffer{}
		mutex := &sync.Mutex{}
		writer := &boundedWriter{
			writer: &threadSafeWriter{
				buf:   buf,
				mutex: mutex,
			},
			maxSize:   5,
			truncated: false,
		}

		n, err := writer.Write([]byte("hello"))
		require.NoError(t, err, "Write failed: %v")

		assert.Equal(t, 5, n)
		assert.Equal(t, "hello", buf.String())
		assert.False(t, writer.truncated)
	})

	t.Run("Write exceeding limit with ASCII", func(t *testing.T) {
		buf := &bytes.Buffer{}
		mutex := &sync.Mutex{}
		writer := &boundedWriter{
			writer: &threadSafeWriter{
				buf:   buf,
				mutex: mutex,
			},
			maxSize:   5,
			truncated: false,
		}

		n, err := writer.Write([]byte("hello world"))
		require.NoError(t, err, "Write failed: %v")

		assert.Len(t, "hello world", n)
		assert.Equal(t, "world", buf.String())
		assert.True(t, writer.truncated)

		n2, err := writer.Write([]byte(" more"))
		require.NoError(t, err, "Second write failed: %v")

		assert.Equal(t, 5, n2)
		assert.Equal(t, " more", buf.String())
	})

	t.Run("Write exceeding limit with UTF-8 characters", func(t *testing.T) {
		buf := &bytes.Buffer{}
		mutex := &sync.Mutex{}
		writer := &boundedWriter{
			writer: &threadSafeWriter{
				buf:   buf,
				mutex: mutex,
			},
			maxSize:   5,
			truncated: false,
		}

		n, err := writer.Write([]byte("héllo world"))
		require.NoError(t, err, "Write failed: %v")

		assert.Len(t, "héllo world", n)
		assert.Equal(t, "world", buf.String())
		assert.True(t, writer.truncated)
	})

	t.Run("Write exceeding limit inside first UTF-8 rune keeps valid boundary", func(t *testing.T) {
		buf := &bytes.Buffer{}
		mutex := &sync.Mutex{}
		writer := &boundedWriter{
			writer: &threadSafeWriter{
				buf:   buf,
				mutex: mutex,
			},
			maxSize:   1,
			truncated: false,
		}

		n, err := writer.Write([]byte("é"))
		require.NoError(t, err, "Write failed: %v")

		assert.Len(t, "é", n)
		assert.Equal(t, 0, buf.Len())
		assert.True(t, writer.truncated)
	})

	t.Run("Write with zero limit", func(t *testing.T) {
		buf := &bytes.Buffer{}
		mutex := &sync.Mutex{}
		writer := &boundedWriter{
			writer: &threadSafeWriter{
				buf:   buf,
				mutex: mutex,
			},
			maxSize:   0,
			truncated: false,
		}

		n, err := writer.Write([]byte("hello"))
		require.NoError(t, err, "Write failed: %v")

		assert.Equal(t, 5, n)
		assert.Empty(t, buf.String())
		assert.True(t, writer.truncated)
	})

	t.Run("Multiple writes within limit", func(t *testing.T) {
		buf := &bytes.Buffer{}
		mutex := &sync.Mutex{}
		writer := &boundedWriter{
			writer: &threadSafeWriter{
				buf:   buf,
				mutex: mutex,
			},
			maxSize:   11,
			truncated: false,
		}

		n1, err := writer.Write([]byte("hello"))
		require.NoError(t, err, "First write failed: %v")

		assert.Equal(t, 5, n1)

		n2, err := writer.Write([]byte(" "))
		require.NoError(t, err, "Second write failed: %v")

		assert.Equal(t, 1, n2)

		n3, err := writer.Write([]byte("world"))
		require.NoError(t, err, "Third write failed: %v")

		assert.Equal(t, 5, n3)

		assert.Equal(t, "hello world", buf.String())
		assert.False(t, writer.truncated)
	})
}

func TestTruncateOutputUTF8Boundary(t *testing.T) {
	buf := &bytes.Buffer{}
	mutex := &sync.Mutex{}

	buf.WriteString("abécd")

	out, truncated := truncateOutput(buf, 3, mutex)
	require.True(t, truncated, "Expected truncation to be reported")
	require.Equal(t, "cd", out, "Expected UTF-8 safe suffix 'cd', got %q", out)
	require.Equal(t, "cd", buf.String(), "Expected buffer to be rewritten with UTF-8 safe suffix, got %q", buf.String())
}

func TestReadTextFileWindow(t *testing.T) {
	t.Parallel()

	testPath := filepath.Join(t.TempDir(), "window.txt")
	content := "line1\nline2\nline3\n"
	if err := os.WriteFile(testPath, []byte(content), 0o644); err != nil {
		require.NoError(t, err, "failed to write test file: %v")
	}

	tests := []struct {
		name      string
		startLine int
		limit     *int
		expected  string
	}{
		{
			name:      "keep suffix when no limit",
			startLine: 1,
			limit:     nil,
			expected:  "line2\nline3\n",
		},
		{
			name:      "apply limit",
			startLine: 1,
			limit:     new(1),
			expected:  "line2",
		},
		{
			name:      "start beyond content",
			startLine: 10,
			limit:     nil,
			expected:  "",
		},
		{
			name:      "zero limit returns empty",
			startLine: 0,
			limit:     new(0),
			expected:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := readTextFileWindow(testPath, tc.startLine, tc.limit, maxACPTextFileBytes)
			require.NoError(t, err, "readTextFileWindow failed: %v")
			require.Equal(t, tc.expected, got, "expected %q, got %q", tc.expected, got)
		})
	}

	t.Run("enforces byte limit", func(t *testing.T) {
		t.Parallel()

		tooLargePath := filepath.Join(t.TempDir(), "too-large.txt")
		tooLarge := strings.Repeat("x", maxACPTextFileBytes+1)
		if err := os.WriteFile(tooLargePath, []byte(tooLarge), 0o644); err != nil {
			require.NoError(t, err, "failed to write large test file: %v")
		}

		_, err := readTextFileWindow(tooLargePath, 0, nil, maxACPTextFileBytes)
		require.Error(t, err, "expected byte-limit error, got nil")

		require.ErrorContains(t, err, "file content too large")
	})
}

func TestACPAliasCollisionFixed(t *testing.T) {
	fakeBin := t.TempDir()
	agentBin := "agent"
	if runtime.GOOS == "windows" {
		agentBin += ".exe"
	}
	agentPath := filepath.Join(fakeBin, agentBin)
	if err := os.WriteFile(agentPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		require.NoError(t, err, "failed to create fake agent binary: %v")
	}
	t.Setenv("PATH", fakeBin)

	cfg := &config.Config{
		ACP: &config.ACPAgentConfig{
			Name:    "agent",
			Command: "acp-agent",
		},
	}

	resolved, err := GetAvailableWithConfig("", "cursor", cfg)
	require.NoError(t, err, "GetAvailableWithConfig failed: %v")

	require.Equal(t, "cursor", resolved.Name(), "resolved agent name")
}

func TestGetAvailableWithConfigUnknownAgentErrors(t *testing.T) {
	cfg := &config.Config{}

	_, err := GetAvailableWithConfig("", "typo-agent", cfg)
	require.Error(t, err, "Expected error for unknown agent name")
	require.ErrorContains(t, err, "unknown agent")
}

func TestGetAvailableWithConfigPassesBackupsThrough(t *testing.T) {
	fakeBin := t.TempDir()
	binName := "gemini"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	geminiPath := filepath.Join(fakeBin, binName)
	err := os.WriteFile(geminiPath, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	require.NoError(t, err, "create fake gemini binary: %v", err)
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"codex":  NewCodexAgent("definitely-not-on-path"),
		"gemini": NewGeminiAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	cfg := &config.Config{}
	resolved, err := GetAvailableWithConfig("", "codex", cfg, "gemini")
	require.NoError(t, err, "expected fallback, got error: %v", err)
	assert.Equal(t, "gemini", resolved.Name(), "expected backup agent to be selected")
}

func TestACPNameDoesNotMatchCanonicalRequest(t *testing.T) {
	fakeBin := t.TempDir()
	binName := "claude"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	claudePath := filepath.Join(fakeBin, binName)
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		require.NoError(t, err, "failed to create fake claude binary: %v")
	}
	t.Setenv("PATH", fakeBin)

	cfg := &config.Config{
		ACP: &config.ACPAgentConfig{
			Name:    "claude",
			Command: defaultACPCommand,
		},
	}

	resolved, err := GetAvailableWithConfig("", "claude-code", cfg)
	require.NoError(t, err, "GetAvailableWithConfig failed: %v")

	assert.NotEqual(t, "acp", resolved.Name(), "Request for 'claude-code' should not route to ACP when acp.name='claude'")
	assert.Equal(t, "claude-code", resolved.Name(), "Expected claude-code agent, got %q", resolved.Name())
}

func TestGetAvailableWithConfigAppliesClaudeCodeCmd(t *testing.T) {
	fakeBin := t.TempDir()
	binName := "claude-wrapper"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	wrapperPath := filepath.Join(fakeBin, binName)
	err := os.WriteFile(
		wrapperPath,
		[]byte("#!/bin/sh\nexit 0\n"), 0o755,
	)
	require.NoError(t, err)
	t.Setenv("PATH", fakeBin)

	cfg := &config.Config{ClaudeCodeCmd: wrapperPath}

	resolved, err := GetAvailableWithConfig("", "claude-code", cfg)
	require.NoError(t, err)

	ca, ok := resolved.(CommandAgent)
	require.True(t, ok, "resolved agent should implement CommandAgent")
	assert.Equal(t, "claude-code", resolved.Name())
	assert.Equal(t, wrapperPath, ca.CommandName(),
		"configured claude_code_cmd should override the default command")
}

func TestGetAvailableWithConfigUsesConfigCmdForAvailability(t *testing.T) {
	// Default "claude" is NOT in PATH, but the configured wrapper is.
	// GetAvailableWithConfig should find the agent available via config.
	fakeBin := t.TempDir()
	wrapper := "claude-wrapper"
	if runtime.GOOS == "windows" {
		wrapper += ".exe"
	}
	err := os.WriteFile(
		filepath.Join(fakeBin, wrapper),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755,
	)
	require.NoError(t, err)
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"claude-code": NewClaudeAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	cfg := &config.Config{
		ClaudeCodeCmd: filepath.Join(fakeBin, wrapper),
	}

	resolved, err := GetAvailableWithConfig("", "claude-code", cfg)
	require.NoError(t, err, "agent should be available via configured command")

	assert.Equal(t, "claude-code", resolved.Name())
	ca, ok := resolved.(CommandAgent)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(fakeBin, wrapper), ca.CommandName())
}

func TestGetAvailableWithConfigBackupUsesConfigCmd(t *testing.T) {
	// Neither default "codex" nor "claude" in PATH, but the configured
	// claude_code_cmd is. Preferred=codex should fall back to the
	// backup claude-code via its configured command.
	fakeBin := t.TempDir()
	wrapper := "my-claude"
	if runtime.GOOS == "windows" {
		wrapper += ".exe"
	}
	err := os.WriteFile(
		filepath.Join(fakeBin, wrapper),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755,
	)
	require.NoError(t, err)
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"codex":       NewCodexAgent(""),
		"claude-code": NewClaudeAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	cfg := &config.Config{
		ClaudeCodeCmd: filepath.Join(fakeBin, wrapper),
	}

	resolved, err := GetAvailableWithConfig(
		"", "codex", cfg, "claude-code",
	)
	require.NoError(t, err,
		"backup agent should be available via configured command")
	assert.Equal(t, "claude-code", resolved.Name())

	ca, ok := resolved.(CommandAgent)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(fakeBin, wrapper), ca.CommandName())
}

func TestGetAvailableWithConfigCodexCmd(t *testing.T) {
	fakeBin := t.TempDir()
	wrapper := "custom-codex"
	if runtime.GOOS == "windows" {
		wrapper += ".exe"
	}
	err := os.WriteFile(
		filepath.Join(fakeBin, wrapper),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755,
	)
	require.NoError(t, err)
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"codex": NewCodexAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	cfg := &config.Config{
		CodexCmd: filepath.Join(fakeBin, wrapper),
	}

	resolved, err := GetAvailableWithConfig("", "codex", cfg)
	require.NoError(t, err)
	assert.Equal(t, "codex", resolved.Name())

	ca, ok := resolved.(CommandAgent)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(fakeBin, wrapper), ca.CommandName())
}

func TestGetAvailableWithConfigCursorCmd(t *testing.T) {
	fakeBin := t.TempDir()
	wrapper := "custom-cursor"
	if runtime.GOOS == "windows" {
		wrapper += ".exe"
	}
	err := os.WriteFile(
		filepath.Join(fakeBin, wrapper),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755,
	)
	require.NoError(t, err)
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"cursor": NewCursorAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	cfg := &config.Config{
		CursorCmd: filepath.Join(fakeBin, wrapper),
	}

	resolved, err := GetAvailableWithConfig("", "cursor", cfg)
	require.NoError(t, err)
	assert.Equal(t, "cursor", resolved.Name())

	ca, ok := resolved.(CommandAgent)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(fakeBin, wrapper), ca.CommandName())
}

func TestGetAvailableWithConfigPiCmd(t *testing.T) {
	fakeBin := t.TempDir()
	wrapper := "custom-pi"
	if runtime.GOOS == "windows" {
		wrapper += ".exe"
	}
	err := os.WriteFile(
		filepath.Join(fakeBin, wrapper),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755,
	)
	require.NoError(t, err)
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"pi": NewPiAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	cfg := &config.Config{
		PiCmd: filepath.Join(fakeBin, wrapper),
	}

	resolved, err := GetAvailableWithConfig("", "pi", cfg)
	require.NoError(t, err)
	assert.Equal(t, "pi", resolved.Name())

	ca, ok := resolved.(CommandAgent)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(fakeBin, wrapper), ca.CommandName())
}

func TestGetAvailableWithConfigOpenCodeCmd(t *testing.T) {
	fakeBin := t.TempDir()
	wrapper := "custom-opencode"
	if runtime.GOOS == "windows" {
		wrapper += ".exe"
	}
	err := os.WriteFile(
		filepath.Join(fakeBin, wrapper),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755,
	)
	require.NoError(t, err)
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"opencode": NewOpenCodeAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	cfg := &config.Config{
		OpenCodeCmd: filepath.Join(fakeBin, wrapper),
	}

	resolved, err := GetAvailableWithConfig("", "opencode", cfg)
	require.NoError(t, err)
	assert.Equal(t, "opencode", resolved.Name())

	ca, ok := resolved.(CommandAgent)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(fakeBin, wrapper), ca.CommandName())
}

func TestGetAvailableWithConfigACPFallbackBackupUsesConfigCmd(t *testing.T) {
	// Configured ACP alias is requested but ACP command is
	// unavailable. The backup agent's default command is also
	// unavailable, but its *_cmd override points to a real binary.
	// The config-aware backup path should find it.
	fakeBin := t.TempDir()
	wrapper := "claude-wrapper"
	if runtime.GOOS == "windows" {
		wrapper += ".exe"
	}
	err := os.WriteFile(
		filepath.Join(fakeBin, wrapper),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755,
	)
	require.NoError(t, err)
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"claude-code": NewClaudeAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	cfg := &config.Config{
		ACP: &config.ACPAgentConfig{
			Name:    "my-acp",
			Command: "nonexistent-acp-binary",
		},
		ClaudeCodeCmd: filepath.Join(fakeBin, wrapper),
	}

	resolved, err := GetAvailableWithConfig(
		"", "my-acp", cfg, "claude-code",
	)
	require.NoError(t, err,
		"backup should resolve via config cmd when ACP is unavailable")
	assert.Equal(t, "claude-code", resolved.Name())

	ca, ok := resolved.(CommandAgent)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(fakeBin, wrapper), ca.CommandName())
}

func TestGetAvailableWithConfigEmptyPreferredBackupUsesConfigCmd(t *testing.T) {
	// preferred="" with a backup agent whose default command isn't in
	// PATH but whose configured command is. The backup should still
	// be resolved via the config override.
	fakeBin := t.TempDir()
	wrapper := "claude-wrapper"
	if runtime.GOOS == "windows" {
		wrapper += ".exe"
	}
	err := os.WriteFile(
		filepath.Join(fakeBin, wrapper),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755,
	)
	require.NoError(t, err)
	t.Setenv("PATH", fakeBin)

	originalRegistry := registry
	registry = map[string]Agent{
		"claude-code": NewClaudeAgent(""),
	}
	t.Cleanup(func() { registry = originalRegistry })

	cfg := &config.Config{
		ClaudeCodeCmd: filepath.Join(fakeBin, wrapper),
	}

	resolved, err := GetAvailableWithConfig("", "", cfg, "claude-code")
	require.NoError(t, err,
		"backup agent should be available via config cmd even with empty preferred")
	assert.Equal(t, "claude-code", resolved.Name())

	ca, ok := resolved.(CommandAgent)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(fakeBin, wrapper), ca.CommandName())
}
