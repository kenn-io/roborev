package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

type strictReviewTestAgent struct{ *TestAgent }

func (a *strictReviewTestAgent) WithStrictReview(config.ReviewToolPolicy, ReviewMCP) Agent { return a }

// writeOperatorMCPConfig writes an operator server file with one server a
// review can be granted and one it never is.
func writeOperatorMCPConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "servers.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"mcpServers":{
		"sources":{"command":"source-reader","args":["--stdio"]},
		"unused":{"type":"http","url":"https://example.invalid/mcp"}}}`), 0o600))
	return path
}

func TestSelectReviewMCP(t *testing.T) {
	cfg := &config.Config{ReviewMCPConfig: writeOperatorMCPConfig(t), ReviewMCPAllowedTools: []string{"sources/read", "sources/find", "unused/ping"}}
	sel, err := selectReviewMCP(cfg, &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"sources": {"find", "read"}}})
	require.NoError(t, err)
	mcp, cleanup, err := writeReviewMCPConfig(sel)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	data, err := os.ReadFile(mcp.ConfigPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{"sources":{"command":"source-reader","args":["--stdio"]}}}`, string(data))
	assert.Equal(t, []string{"sources"}, mcp.Servers)
	assert.Equal(t, []string{"mcp__sources__find", "mcp__sources__read"}, mcp.Tools)

	// Without grants the operator file is never read, so a missing one is fine.
	cfg.ReviewMCPConfig = filepath.Join(t.TempDir(), "missing.json")
	sel, err = selectReviewMCP(cfg, &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{}})
	require.NoError(t, err)
	mcp, cleanup, err = writeReviewMCPConfig(sel)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	data, err = os.ReadFile(mcp.ConfigPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{}}`, string(data))
	assert.Empty(t, mcp.Servers)
	assert.Empty(t, mcp.Tools)
}

func TestSelectReviewMCPRejects(t *testing.T) {
	operator := writeOperatorMCPConfig(t)
	read := map[string][]string{"sources": {"read"}}
	for _, tt := range []struct {
		name    string
		cfg     *config.Config
		grants  map[string][]string
		wantErr string
	}{
		{"no ceiling", &config.Config{ReviewMCPConfig: operator}, read, `"sources/read" is not in review_mcp_allowed_tools`},
		{"other tool on the same server", &config.Config{ReviewMCPConfig: operator, ReviewMCPAllowedTools: []string{"sources/find"}}, read, `"sources/read" is not in`},
		{"wildcard ceiling", &config.Config{ReviewMCPConfig: operator, ReviewMCPAllowedTools: []string{"sources/*"}}, read, `"sources/read" is not in`},
		{"server missing from file", &config.Config{ReviewMCPConfig: operator, ReviewMCPAllowedTools: []string{"archive/read"}}, map[string][]string{"archive": {"read"}}, `has no server "archive"`},
		{"relative file", &config.Config{ReviewMCPConfig: "servers.json", ReviewMCPAllowedTools: []string{"sources/read"}}, read, "absolute path"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := selectReviewMCP(tt.cfg, &config.ReviewToolPolicy{Mode: "read-only", MCP: tt.grants})
			require.ErrorIs(t, err, ErrReviewToolPolicy)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestValidateReviewToolSelection(t *testing.T) {
	strict := &config.ReviewToolPolicy{Mode: "read-only"}
	capable := &strictReviewTestAgent{NewTestAgent()}
	require.NoError(t, ValidateReviewToolSelection(nil, NewTestAgent(), true))
	require.NoError(t, ValidateReviewToolSelection(strict, capable, false))
	require.ErrorIs(t, ValidateReviewToolSelection(strict, capable, true), ErrReviewToolPolicy)
	require.ErrorContains(t, ValidateReviewToolSelection(strict, capable, true), "agentic")
	require.ErrorContains(t, ValidateReviewToolSelection(&config.ReviewToolPolicy{}, capable, false), "mode")
	withMCP := &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"sources": {"read"}}}
	require.NoError(t, ValidateReviewToolSelection(withMCP, capable, false))
	claude, err := Get("claude-code")
	require.NoError(t, err)
	require.NoError(t, ValidateReviewToolSelection(strict, claude, false))
	require.NoError(t, ValidateReviewToolSelection(withMCP, claude, false))
	for _, name := range []string{"codex", "test"} {
		a, err := Get(name)
		require.NoError(t, err)
		require.ErrorContains(t, ValidateReviewToolSelection(strict, a, false), "does not support strict review tools", name)
	}
}

func TestValidateReviewToolBackup(t *testing.T) {
	strict := &config.ReviewToolPolicy{Mode: "read-only"}
	resolution := WorkflowConfig{RepoConfig: &config.RepoConfig{}, GlobalConfig: config.DefaultConfig(), BackupAgent: "test"}
	require.NoError(t, ValidateReviewToolBackup(nil, resolution, "claude-code"))
	require.ErrorContains(t, ValidateReviewToolBackup(strict, resolution, "claude-code"), "invalid backup agent")
	resolution.BackupAgent = "claude"
	require.NoError(t, ValidateReviewToolBackup(strict, resolution, "claude-code"), "a backup that is the selected agent adds nothing to check")
	resolution.BackupAgent = "no-such-backup"
	require.ErrorContains(t, ValidateReviewToolBackup(strict, resolution, "claude-code"), "resolve backup agent")
}
