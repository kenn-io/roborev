package agent

import (
	"context"
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

const claudeStrictHelp = "--restricted --tools --allowedTools, --allowed-tools --strict-mcp-config --mcp-config " +
	"--permission-mode --permission-prompts --no-session-persistence --disable-slash-commands --effort --json-schema"

const claudeStrictInitLine = `{"type":"system","subtype":"init","permissionMode":"dontAsk","tools":["Read","Glob","Grep","StructuredOutput"],"mcp_servers":[]}`

func TestClaudeStrictReviewRunsLockedDownSession(t *testing.T) {
	mock := mockAgentCLI(t, MockCLIOpts{
		HelpOutput:  claudeStrictHelp,
		CaptureArgs: true,
		StdoutLines: []string{claudeStrictInitLine, `{"type":"result","result":"{\"findings\":[]}"}`},
	})
	base := NewClaudeAgent(mock.CmdPath).WithModel("opus").WithReasoning(ReasoningThorough).(*ClaudeAgent).WithSessionID("old-session")
	prepared, cleanup, err := PrepareReviewToolPolicy(base, &config.ReviewToolPolicy{Mode: "read-only"})
	require.NoError(t, err)
	mcpConfig := prepared.(*ClaudeAgent).StrictReviewMCPConfig()
	data, err := os.ReadFile(mcpConfig)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{}}`, string(data))

	// A session set after preparation must not resume either.
	strict := prepared.(*ClaudeAgent).WithSessionID("later-session").(*ClaudeAgent)
	out, err := strict.ReviewWithSchema(context.Background(), t.TempDir(), "HEAD", "prompt", jsontext.Value(`{"type":"object"}`), nil)
	require.NoError(t, err)
	assert.JSONEq(t, `{"findings":[]}`, string(out))
	assert.Equal(t, []string{
		"-p", "--verbose", "--output-format", "stream-json",
		"--restricted", "--tools", "Glob,Grep,Read", "--allowedTools", "Glob,Grep,Read",
		"--strict-mcp-config", "--mcp-config", mcpConfig,
		"--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--no-session-persistence", "--disable-slash-commands",
		"--model", "opus", "--effort", "high",
		"--json-schema", `{"type":"object"}`,
	}, readMockArgs(t, mock.ArgsFile))

	cleanup()
	_, err = os.Stat(mcpConfig)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestClaudeStrictReviewRequiresStrictFlags(t *testing.T) {
	mock := mockAgentCLI(t, MockCLIOpts{
		HelpOutput:  strings.Replace(claudeStrictHelp, "--restricted ", "", 1),
		CaptureArgs: true,
		StdoutLines: []string{claudeStrictInitLine},
	})
	strict := NewClaudeAgent(mock.CmdPath).WithStrictReview(config.ReviewToolPolicy{Mode: "read-only"}, "mcp.json")

	_, err := strict.Review(context.Background(), t.TempDir(), "HEAD", "prompt", nil)

	require.ErrorIs(t, err, ErrReviewToolPolicy)
	require.ErrorContains(t, err, "--restricted; upgrade claude")
	assert.NoFileExists(t, mock.ArgsFile, "claude must not run without the strict flags")
}

func TestParseClaudeStrictStream(t *testing.T) {
	const result = `{"type":"result","result":"review text"}`
	init := func(fields string) string {
		return `{"type":"system","subtype":"init",` + fields + `}`
	}
	tests := []struct {
		name    string
		stream  []string
		wantErr string
	}{
		{name: "exact startup", stream: []string{
			`{"type":"system","subtype":"hook_started"}`,
			init(`"permissionMode":"dontAsk","tools":["Grep","Read","Glob"],"mcp_servers":[]`), result,
		}},
		{name: "extra tool", stream: []string{
			init(`"permissionMode":"dontAsk","tools":["Read","Glob","Grep","Bash"],"mcp_servers":[]`), result,
		}, wantErr: "started with tools [Bash Glob Grep Read]"},
		{name: "missing tool", stream: []string{
			init(`"permissionMode":"dontAsk","tools":["Read"],"mcp_servers":[]`), result,
		}, wantErr: "started with tools [Read]"},
		{name: "wrong permission mode", stream: []string{
			init(`"permissionMode":"bypassPermissions","tools":["Read","Glob","Grep"],"mcp_servers":[]`), result,
		}, wantErr: `permission mode "bypassPermissions"`},
		{name: "MCP server", stream: []string{
			init(`"permissionMode":"dontAsk","tools":["Read","Glob","Grep"],"mcp_servers":[{"name":"ambient","status":"connected"}]`), result,
		}, wantErr: `MCP server "ambient"`},
		{name: "output before init", stream: []string{
			result, init(`"permissionMode":"dontAsk","tools":["Read","Glob","Grep"],"mcp_servers":[]`),
		}, wantErr: "result output before its init event"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseClaudeStrictStream(strings.NewReader(strings.Join(tt.stream, "\n")), nil, claudeStrictNativeTools, false)
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, "review text", got)
				return
			}
			require.ErrorIs(t, err, ErrReviewToolPolicy)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}

	_, err := parseClaudeStrictStream(strings.NewReader(`{"type":"system","subtype":"hook_started"}`), nil, claudeStrictNativeTools, false)
	require.ErrorContains(t, err, "did not report its startup tools")
}

func TestCheckStrictReviewPrepared(t *testing.T) {
	policy := &config.ReviewToolPolicy{Mode: "read-only"}
	claude := NewClaudeAgent("claude")
	require.NoError(t, CheckStrictReviewPrepared(nil, claude))
	require.ErrorIs(t, CheckStrictReviewPrepared(policy, claude), ErrReviewToolPolicy)
	require.NoError(t, CheckStrictReviewPrepared(policy, claude.WithStrictReview(*policy, "mcp.json")))
}

func TestClaudeStrictReviewStopsOnBadStartup(t *testing.T) {
	helpFile := filepath.Join(t.TempDir(), "help.txt")
	require.NoError(t, os.WriteFile(helpFile, []byte(claudeStrictHelp), 0o600))
	// The CLI would keep running after a startup report that breaks the policy.
	cmd := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; --help) cat "`+helpFile+`"; exit 0;; esac
echo '{"type":"system","subtype":"init","permissionMode":"dontAsk","tools":["Read","Glob","Grep","Bash"],"mcp_servers":[]}'
exec sleep 60
`)
	strict := NewClaudeAgent(cmd).WithStrictReview(config.ReviewToolPolicy{Mode: "read-only"}, "mcp.json")

	_, err := strict.Review(context.Background(), t.TempDir(), "HEAD", "prompt", nil)

	require.ErrorIs(t, err, ErrReviewToolPolicy)
	assert.ErrorContains(t, err, "started with tools [Bash Glob Grep Read]")
}
