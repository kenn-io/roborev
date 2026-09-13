package main

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		t.Fatal("mcp serve did not exit after stdin closed")
	}
	assert.NotContains(stderr.String(), "Error")
}
