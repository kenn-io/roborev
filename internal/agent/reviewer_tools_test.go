package agent

import (
	"context"
	"encoding/json/v2"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testReviewerTools = ReviewerTools{
	URL:       "http://127.0.0.1:7373/mcp/review",
	Token:     "reviewer-token",
	ToolNames: []string{"branch_findings", "git_blame"},
}

func TestClaudeReviewConnectsOnlyTheReviewerServer(t *testing.T) {
	assert := assert.New(t)
	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureArgs: true,
		StdoutLines: []string{`{"type":"result","result":"ok"}`},
	})
	ctx := WithReviewerTools(context.Background(), testReviewerTools)

	_, err := NewClaudeAgent(mock.CmdPath).Review(ctx, t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	args := readMockArgs(t, mock.ArgsFile)
	assert.Equal(
		"Read,Glob,Grep,mcp__roborev_review__branch_findings,mcp__roborev_review__git_blame",
		argAfter(args, "--allowedTools"),
	)
	assertContainsArg(t, args, "--strict-mcp-config")
	assertNotContainsArg(t, args, "--disallowedTools")
	assertNotContainsArg(t, args, "--permission-mode")
	for _, arg := range args {
		assert.NotContains(arg, testReviewerTools.Token, "the token must stay out of the process arguments")
	}
	_, statErr := os.Stat(argAfter(args, "--mcp-config"))
	assert.ErrorIs(statErr, os.ErrNotExist, "the config file is removed after the run")
}

func TestClaudeReviewWithoutReviewerToolsIsUnchanged(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no tools": context.Background(),
		"planning": context.WithValue(
			WithReviewerTools(context.Background(), testReviewerTools), planningContextKey{}, true),
	} {
		t.Run(name, func(t *testing.T) {
			mock := mockAgentCLI(t, MockCLIOpts{
				CaptureArgs: true,
				StdoutLines: []string{`{"type":"result","result":"ok"}`},
			})
			_, err := NewClaudeAgent(mock.CmdPath).Review(ctx, t.TempDir(), "deadbeef", "prompt", nil)
			require.NoError(t, err)

			args := readMockArgs(t, mock.ArgsFile)
			assertNotContainsArg(t, args, "--mcp-config")
			assertNotContainsArg(t, args, "--strict-mcp-config")
			assert.Equal(t, "Read,Glob,Grep", argAfter(args, "--allowedTools"))
		})
	}
}

func TestClaudeReviewerConfigCarriesTheEndpoint(t *testing.T) {
	path, cleanup, err := writeClaudeReviewerConfig(testReviewerTools)
	require.NoError(t, err)
	defer cleanup()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var config struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(data, &config))
	require.Len(t, config.MCPServers, 1)
	server := config.MCPServers[ReviewerToolsServer]
	assert.Equal(t, "http", server.Type)
	assert.Equal(t, testReviewerTools.URL, server.URL)
	assert.Equal(t, map[string]string{"Authorization": "Bearer reviewer-token"}, server.Headers)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestCodexReviewAddsReviewerServerAndKeepsSandbox(t *testing.T) {
	assert := assert.New(t)
	a, mock := setupMockCodex(t, false, MockCLIOpts{
		HelpOutput:  "usage --sandbox",
		CaptureArgs: true,
		CaptureEnv:  true,
		StdoutLines: []string{`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`},
	})
	ctx := WithReviewerTools(context.Background(), testReviewerTools)

	_, err := a.Review(ctx, t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	args := readMockArgs(t, mock.ArgsFile)
	assert.Equal("read-only", argAfter(args, "--sandbox"))
	assertNotContainsArg(t, args, codexDangerousFlag)
	assertContainsArg(t, args, `mcp_servers.roborev_review.url="http://127.0.0.1:7373/mcp/review"`)
	assertContainsArg(t, args, `mcp_servers.roborev_review.bearer_token_env_var="ROBOREV_REVIEW_MCP_TOKEN"`)
	assertContainsArg(t, args, `mcp_servers.roborev_review.enabled_tools=["branch_findings","git_blame"]`)
	assert.Equal("-", args[len(args)-1], "the prompt is still read from stdin")
	for _, arg := range args {
		assert.NotContains(arg, testReviewerTools.Token)
	}
	assert.Contains(readMockEnv(t, mock.EnvFile), "ROBOREV_REVIEW_MCP_TOKEN=reviewer-token")
}
