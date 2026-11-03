package agent

import (
	"context"
	"encoding/json/jsontext"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

const claudeStrictHelp = "--restricted --tools --allowedTools, --allowed-tools --strict-mcp-config --mcp-config " +
	"--permission-mode --permission-prompts --no-session-persistence --disable-slash-commands --effort --json-schema --settings"

const claudeStrictInitLine = `{"type":"system","subtype":"init","permissionMode":"dontAsk","tools":["Read","Glob","Grep","StructuredOutput"],"mcp_servers":[]}`

func TestClaudeStrictReviewRunsLockedDownSession(t *testing.T) {
	// The selected server also offers an ungranted tool; --allowedTools denies it.
	initLine := `{"type":"system","subtype":"init","permissionMode":"dontAsk",` +
		`"tools":["Read","Glob","Grep","StructuredOutput","mcp__sources__read","mcp__sources__find"],` +
		`"mcp_servers":[{"name":"sources","status":"connected"}]}`
	mock := mockAgentCLI(t, MockCLIOpts{
		HelpOutput:  claudeStrictHelp,
		CaptureArgs: true,
		StdoutLines: []string{initLine, `{"type":"result","result":"{\"findings\":[]}"}`},
	})
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	require.NoError(t, os.WriteFile(filepath.Join(userDir, "settings.json"), []byte(`{
		"env": {"ANTHROPIC_BASE_URL": "https://gateway.example", "ANTHROPIC_AUTH_TOKEN": "synthetic-token"},
		"apiKeyHelper": "/usr/local/bin/synthetic-key",
		"hooks": {"PreToolUse": []},
		"permissions": {"allow": ["Bash"]},
		"enabledPlugins": {"synthetic@market": true}
	}`), 0o600))
	cfg := &config.Config{ReviewMCPConfig: writeOperatorMCPConfig(t), ReviewMCPAllowedTools: []string{"sources/read"}}
	policy := &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"sources": {"read"}}}
	base := NewClaudeAgent(mock.CmdPath).WithModel("opus").WithReasoning(ReasoningThorough).(*ClaudeAgent).WithSessionID("old-session")
	prepared, cleanup, err := PrepareReviewToolPolicy(base, policy, cfg)
	require.NoError(t, err)
	mcpConfig := prepared.(*ClaudeAgent).StrictReviewMCPConfig()
	data, err := os.ReadFile(mcpConfig)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{"sources":{"command":"source-reader","args":["--stdio"]}}}`, string(data))
	// Only credential and provider keys survive; hooks, permissions and
	// plugins from user settings must never reach the strict session.
	settings := filepath.Join(filepath.Dir(mcpConfig), "settings.json")
	data, err = os.ReadFile(settings)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"env": {"ANTHROPIC_BASE_URL": "https://gateway.example", "ANTHROPIC_AUTH_TOKEN": "synthetic-token"},
		"apiKeyHelper": "/usr/local/bin/synthetic-key"
	}`, string(data))
	info, err := os.Stat(settings)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// A session set after preparation must not resume either.
	strict := prepared.(*ClaudeAgent).WithSessionID("later-session").(*ClaudeAgent)
	out, err := strict.ReviewWithSchema(t.Context(), t.TempDir(), "HEAD", "prompt", jsontext.Value(`{"type":"object"}`), nil)
	require.NoError(t, err)
	assert.JSONEq(t, `{"findings":[]}`, string(out))
	assert.Equal(t, []string{
		"-p", "--verbose", "--output-format", "stream-json",
		"--restricted", "--tools", "Glob,Grep,Read", "--allowedTools", "Glob,Grep,Read,mcp__sources__read",
		"--strict-mcp-config", "--mcp-config", mcpConfig,
		"--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--no-session-persistence", "--disable-slash-commands",
		"--settings", settings,
		"--model", "opus", "--effort", "high",
		"--json-schema", `{"type":"object"}`,
	}, readMockArgs(t, mock.ArgsFile))

	cleanup()
	_, err = os.Stat(mcpConfig)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(settings)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestClaudeStrictReviewWithoutUserSettingsPassesNoSettings(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	prepared, cleanup, err := PrepareReviewToolPolicy(NewClaudeAgent("claude"), &config.ReviewToolPolicy{Mode: "read-only"}, &config.Config{})
	require.NoError(t, err)
	defer cleanup()

	strict := prepared.(*ClaudeAgent)
	assert.NotContains(t, strict.buildStrictArgs(false), "--settings")
	assert.NoFileExists(t, filepath.Join(filepath.Dir(strict.StrictReviewMCPConfig()), "settings.json"))
}

func TestClaudeStrictSchemaReviewKeepsQuotaError(t *testing.T) {
	mock := mockAgentCLI(t, MockCLIOpts{
		HelpOutput: claudeStrictHelp,
		StdoutLines: []string{
			claudeStrictInitLine,
			`{"type":"result","is_error":true,"error":{"message":"You've hit your weekly limit"}}`,
		},
		ExitCode: 1,
	})
	strict := NewClaudeAgent(mock.CmdPath).WithStrictReview(config.ReviewToolPolicy{Mode: "read-only"}, ReviewMCP{ConfigPath: "mcp.json"}).(*ClaudeAgent)

	_, err := strict.ReviewWithSchema(t.Context(), t.TempDir(), "HEAD", "prompt", jsontext.Value(`{"type":"object"}`), nil)

	require.ErrorContains(t, err, "You've hit your weekly limit")
	assert.Equal(t, LimitKindQuota, ClassifyLimit("claude-code", "agent: "+err.Error()).Kind)
}

func TestClaudeStrictReviewRequiresStrictFlags(t *testing.T) {
	mock := mockAgentCLI(t, MockCLIOpts{
		HelpOutput:  strings.Replace(claudeStrictHelp, "--restricted ", "", 1),
		CaptureArgs: true,
		StdoutLines: []string{claudeStrictInitLine},
	})
	strict := NewClaudeAgent(mock.CmdPath).WithStrictReview(config.ReviewToolPolicy{Mode: "read-only"}, ReviewMCP{ConfigPath: "mcp.json"})

	_, err := strict.Review(t.Context(), t.TempDir(), "HEAD", "prompt", nil)

	require.ErrorIs(t, err, ErrReviewToolPolicy)
	require.ErrorContains(t, err, "--restricted; upgrade claude")
	assert.NoFileExists(t, mock.ArgsFile, "claude must not run without the strict flags")
}

func TestParseClaudeStrictStream(t *testing.T) {
	const result = `{"type":"result","result":"review text"}`
	init := func(fields string) string {
		return `{"type":"system","subtype":"init",` + fields + `}`
	}
	granted := append(slices.Clone(claudeStrictNativeTools), "mcp__sources__read")
	sources := []string{"sources"}
	mcpInit := func(tools, servers string) string {
		return init(`"permissionMode":"dontAsk","tools":["Read","Glob","Grep"` + tools + `],"mcp_servers":[` + servers + `]`)
	}
	const connected = `{"name":"sources","status":"connected"}`
	tests := []struct {
		name    string
		stream  []string
		tools   []string
		servers []string
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
		{name: "exact MCP startup", tools: granted, servers: sources, stream: []string{
			mcpInit(`,"mcp__sources__read","mcp__sources__find"`, connected), result,
		}},
		{name: "selected server missing", tools: granted, servers: sources, stream: []string{
			mcpInit(`,"mcp__sources__read"`, ""), result,
		}, wantErr: `without selected MCP server "sources"`},
		{name: "selected server not connected", tools: granted, servers: sources, stream: []string{
			mcpInit(`,"mcp__sources__read"`, `{"name":"sources","status":"failed"}`), result,
		}, wantErr: `MCP server "sources" is "failed"`},
		{name: "granted tool missing", tools: granted, servers: sources, stream: []string{
			mcpInit(`,"mcp__sources__find"`, connected), result,
		}, wantErr: `without granted tool "mcp__sources__read"`},
		{name: "unselected server", tools: granted, servers: sources, stream: []string{
			mcpInit(`,"mcp__sources__read"`, connected+`,{"name":"ambient","status":"connected"}`), result,
		}, wantErr: `MCP server "ambient"`},
		{name: "foreign MCP namespace", tools: granted, servers: sources, stream: []string{
			mcpInit(`,"mcp__sources__read","mcp__ambient__write"`, connected), result,
		}, wantErr: `"mcp__ambient__write" from an unselected MCP server`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools := tt.tools
			if tools == nil {
				tools = claudeStrictNativeTools
			}
			got, err := parseClaudeStrictStream(strings.NewReader(strings.Join(tt.stream, "\n")), nil, tools, tt.servers, false)
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, "review text", got)
				return
			}
			require.ErrorIs(t, err, ErrReviewToolPolicy)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}

	_, err := parseClaudeStrictStream(strings.NewReader(`{"type":"system","subtype":"hook_started"}`), nil, claudeStrictNativeTools, nil, false)
	require.ErrorContains(t, err, "did not report its startup tools")
}

func TestCheckStrictReviewPrepared(t *testing.T) {
	policy := &config.ReviewToolPolicy{Mode: "read-only"}
	claude := NewClaudeAgent("claude")
	require.NoError(t, CheckStrictReviewPrepared(nil, claude))
	require.ErrorIs(t, CheckStrictReviewPrepared(policy, claude), ErrReviewToolPolicy)
	require.NoError(t, CheckStrictReviewPrepared(policy, claude.WithStrictReview(*policy, ReviewMCP{ConfigPath: "mcp.json"})))
}

func TestClaudeStrictReviewReportsPolicyErrorWhenStopped(t *testing.T) {
	helpFile := filepath.Join(t.TempDir(), "help.txt")
	require.NoError(t, os.WriteFile(helpFile, []byte(claudeStrictHelp), 0o600))
	// The CLI is still running when the review stops it, so the run also fails
	// with a canceled context. The review must report the policy error instead.
	cmd := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; --help) cat "`+helpFile+`"; exit 0;; esac
echo '{"type":"system","subtype":"init","permissionMode":"dontAsk","tools":["Read","Glob","Grep","Bash"],"mcp_servers":[]}'
exec sleep 60
`)
	strict := NewClaudeAgent(cmd).WithStrictReview(config.ReviewToolPolicy{Mode: "read-only"}, ReviewMCP{ConfigPath: "mcp.json"})

	_, err := strict.Review(t.Context(), t.TempDir(), "HEAD", "prompt", nil)

	require.ErrorIs(t, err, ErrReviewToolPolicy)
	require.ErrorContains(t, err, "started with tools [Bash Glob Grep Read]")
}

func TestParseClaudeStrictStreamStopsClaudeOnBadStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		// Claude keeps its stdout open after a startup report that breaks the policy.
		stdout, claude := io.Pipe()
		defer claude.Close()
		go func() {
			_, _ = io.WriteString(claude, `{"type":"system","subtype":"init","permissionMode":"dontAsk","tools":["Read","Glob","Grep","Bash"],"mcp_servers":[]}`+"\n")
		}()

		parsed := make(chan error, 1)
		go func() {
			_, err := parseClaudeStrictStreamOrStop(stdout, nil, claudeStrictNativeTools, nil, false, cancel)
			parsed <- err
		}()
		synctest.Wait()

		var err error
		select {
		case err = <-parsed:
		default: // still reading; err stays nil and the check below fails
		}
		require.ErrorIs(t, err, ErrReviewToolPolicy, "the parser must stop at the bad startup report")
		require.ErrorContains(t, err, "started with tools [Bash Glob Grep Read]")
		assert.ErrorIs(t, runCtx.Err(), context.Canceled, "claude must be stopped")
	})
}
