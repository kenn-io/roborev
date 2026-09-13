package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

func TestMCPCmdExposesServeSubcommand(t *testing.T) {
	cmd := mcpCmd()
	serve, _, err := cmd.Find([]string{"serve"})
	require.NoError(t, err)
	assert.Equal(t, "serve", serve.Name())

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "stdio")
	assert.Contains(t, out.String(), "/mcp")
}

func TestMCPServeRejectsPositionalArgs(t *testing.T) {
	cmd := mcpCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"serve", "extra"})
	assert.Error(t, cmd.Execute())
}

// TestMCPServeSpeaksProtocolOverStdio drives the real serve command over the
// process stdio against a mock daemon, proving stdout carries only protocol
// frames and tools read through the daemon HTTP API.
func TestMCPServeSpeaksProtocolOverStdio(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	newMockDaemonBuilder(t).
		WithJobs([]storage.ReviewJob{{ID: 42, GitRef: "abc", Agent: "codex", JobType: "review", Status: storage.JobStatusDone, Prompt: "secret"}}).
		Build()

	stdinR, stdinW, err := os.Pipe()
	require.NoError(err)
	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(err)
	origStdin, origStdout, origLifecycle := os.Stdin, os.Stdout, lifecycleOut
	os.Stdin, os.Stdout = stdinR, stdoutW
	t.Cleanup(func() {
		os.Stdin, os.Stdout, lifecycleOut = origStdin, origStdout, origLifecycle
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stderr bytes.Buffer
	cmd := mcpCmd()
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"serve"})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: stdoutR, Writer: stdinW}, nil)
	require.NoError(err)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "roborev_list_jobs"})
	require.NoError(err)
	require.False(result.IsError, "%v", result.Content)
	text := result.Content[0].(*mcp.TextContent).Text
	assert.Contains(text, `"id":42`)
	assert.NotContains(text, "secret")

	require.NoError(session.Close())
	_ = stdinW.Close()
	var serveErr error
	require.Eventually(func() bool {
		select {
		case serveErr = <-done:
			return true
		default:
			return false
		}
	}, 10*time.Second, 50*time.Millisecond, "mcp serve did not exit after stdin closed")
	require.NoError(serveErr)
	assert.NotContains(stderr.String(), "Error")
}

func stubMCPStatusDiscovery(
	t *testing.T,
	runtimes []*daemon.RuntimeInfo,
	probe func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error),
) {
	t.Helper()
	origList, origProbe := mcpStatusListRuntimes, mcpStatusProbe
	mcpStatusListRuntimes = func() ([]*daemon.RuntimeInfo, error) { return runtimes, nil }
	mcpStatusProbe = probe
	t.Cleanup(func() {
		mcpStatusListRuntimes, mcpStatusProbe = origList, origProbe
	})
	patchServerAddr(t, "")
}

func TestMCPStatusListsAdvertisedListeners(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	stubMCPStatusDiscovery(t,
		[]*daemon.RuntimeInfo{
			{PID: 11, Network: "tcp", Address: "127.0.0.1:7373"},
			{PID: 12, Network: "tcp", Address: "127.0.0.1:7374"},
			{PID: 13, Network: "tcp", Address: "127.0.0.1:7375"},
		},
		func(ep daemon.DaemonEndpoint, _ time.Duration) (*daemon.PingInfo, error) {
			switch ep.Address {
			case "127.0.0.1:7373":
				return &daemon.PingInfo{OK: true, PID: 11, MCPURL: "http://127.0.0.1:7373/mcp"}, nil
			case "127.0.0.1:7374":
				// MCP disabled on this daemon.
				return &daemon.PingInfo{OK: true, PID: 12}, nil
			default:
				// Stale record: a different process answers on the port.
				return &daemon.PingInfo{OK: true, PID: 99, MCPURL: "http://127.0.0.1:7375/mcp"}, nil
			}
		})

	var out bytes.Buffer
	cmd := mcpCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "--json"})
	require.NoError(cmd.Execute())

	var listeners []mcpListenerStatus
	require.NoError(json.Unmarshal(out.Bytes(), &listeners))
	assert.Equal([]mcpListenerStatus{{
		PID: 11, Transport: "http",
		URL: "http://127.0.0.1:7373/mcp", BackendURL: "http://127.0.0.1:7373",
	}}, listeners)

	out.Reset()
	cmd = mcpCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status"})
	require.NoError(cmd.Execute())
	assert.Equal("MCP http://127.0.0.1:7373/mcp (pid 11, daemon http://127.0.0.1:7373)\n", out.String())
}

func TestMCPStatusReportsEmptyListWithoutListeners(t *testing.T) {
	stubMCPStatusDiscovery(t, nil, func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return nil, errors.New("unreachable")
	})

	var out bytes.Buffer
	cmd := mcpCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "--json"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "[]\n", out.String())

	out.Reset()
	cmd = mcpCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "No HTTP MCP listeners are running.\n", out.String())
}

func TestMCPStatusHonorsServerFlag(t *testing.T) {
	var probed []string
	stubMCPStatusDiscovery(t,
		[]*daemon.RuntimeInfo{{PID: 11, Network: "tcp", Address: "127.0.0.1:7373"}},
		func(ep daemon.DaemonEndpoint, _ time.Duration) (*daemon.PingInfo, error) {
			probed = append(probed, ep.Address)
			return &daemon.PingInfo{OK: true, PID: 5, MCPURL: "http://" + ep.Address + "/mcp"}, nil
		})
	patchServerAddr(t, "127.0.0.1:9999")

	var out bytes.Buffer
	cmd := mcpCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "--json"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, []string{"127.0.0.1:9999"}, probed)
	assert.Contains(t, out.String(), `"backend_url":"http://127.0.0.1:9999"`)
}

func TestMCPServeDoesNotStartDaemonForExplicitServer(t *testing.T) {
	origProbe := mcpProbeDaemon
	var probed []string
	mcpProbeDaemon = func(ep daemon.DaemonEndpoint, _ time.Duration) (*daemon.PingInfo, error) {
		probed = append(probed, ep.Address)
		return nil, errors.New("connection refused")
	}
	t.Cleanup(func() { mcpProbeDaemon = origProbe })
	patchServerAddr(t, "127.0.0.1:9999")

	err := ensureMCPDaemon()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not started automatically")
	assert.Equal(t, []string{"127.0.0.1:9999"}, probed)
}

func TestMCPServeRejectsStaleExplicitServer(t *testing.T) {
	origProbe := mcpProbeDaemon
	mcpProbeDaemon = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return &daemon.PingInfo{OK: true, Service: "roborev", Version: "v0.0.1-stale", PID: 7}, nil
	}
	t.Cleanup(func() { mcpProbeDaemon = origProbe })
	patchServerAddr(t, "127.0.0.1:9999")

	err := ensureMCPDaemon()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "v0.0.1-stale")
	assert.Contains(t, err.Error(), "restart")

	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "1")
	require.NoError(t, ensureMCPDaemon())
}
