package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUntrustedCheckoutAgentEnv checks the environment an agent process
// receives in an untrusted checkout: variables that would make an agent CLI
// trust or load the checkout's configuration are removed or overridden.
func TestUntrustedCheckoutAgentEnv(t *testing.T) {
	skipIfWindows(t)
	t.Setenv("COPILOT_ALLOW_ALL", "true")
	t.Setenv("GITHUB_COPILOT_PROMPT_MODE_WORKSPACE_MCP", "true")
	t.Setenv("GEMINI_CLI_TRUST_WORKSPACE", "true")
	t.Setenv("KILO_DISABLE_PROJECT_CONFIG", "0")
	t.Setenv("GROK_FOLDER_TRUST", "0")
	for _, tt := range []struct {
		name      string
		untrusted bool
	}{
		{name: "local"},
		{name: "untrusted", untrusted: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockAgentCLI(t, MockCLIOpts{
				CaptureEnv:  true,
				StdoutLines: []string{`{"type":"text","part":{"type":"text","text":"ok"}}`},
			})
			ctx := t.Context()
			if tt.untrusted {
				ctx = WithUntrustedCheckout(ctx)
			}
			_, err := NewKiloAgent(mock.CmdPath).Review(ctx, t.TempDir(), "HEAD", "prompt", nil)
			require.NoError(t, err)

			env := readMockEnv(t, mock.EnvFile)
			if tt.untrusted {
				assert.NotContains(t, env, "COPILOT_ALLOW_ALL=true")
				assert.NotContains(t, env, "GITHUB_COPILOT_PROMPT_MODE_WORKSPACE_MCP=true")
				assert.NotContains(t, env, "GEMINI_CLI_TRUST_WORKSPACE=true")
				assert.Contains(t, env, "GEMINI_CLI_TRUST_WORKSPACE=false")
				assert.NotContains(t, env, "KILO_DISABLE_PROJECT_CONFIG=0")
				assert.Contains(t, env, "KILO_DISABLE_PROJECT_CONFIG=1")
				assert.Contains(t, env, "OPENCODE_DISABLE_PROJECT_CONFIG=1")
				assert.NotContains(t, env, "GROK_FOLDER_TRUST=0")
				assert.Contains(t, env, "GROK_FOLDER_TRUST=1")
			} else {
				assert.Contains(t, env, "COPILOT_ALLOW_ALL=true")
				assert.Contains(t, env, "GEMINI_CLI_TRUST_WORKSPACE=true")
				assert.Contains(t, env, "KILO_DISABLE_PROJECT_CONFIG=0")
				assert.NotContains(t, env, "OPENCODE_DISABLE_PROJECT_CONFIG=1")
				assert.Contains(t, env, "GROK_FOLDER_TRUST=0")
			}
		})
	}
}

// TestCursorUntrustedCheckoutUsesEmptyDataDir checks that Cursor starts with
// an empty data directory in an untrusted checkout, so a workspace trust grant
// saved in the user's data directory does not apply to it.
func TestCursorUntrustedCheckoutUsesEmptyDataDir(t *testing.T) {
	skipIfWindows(t)
	userData := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(userData, "trusted"), nil, 0o600))
	t.Setenv("CURSOR_DATA_DIR", userData)
	for _, tt := range []struct {
		name      string
		untrusted bool
	}{
		{name: "local"},
		{name: "untrusted", untrusted: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "data")
			command := writeTempCommand(t, `#!/bin/sh
{ echo "$CURSOR_DATA_DIR"; ls -A "$CURSOR_DATA_DIR"; } > '`+capture+`'
echo '{"type":"result","result":"ok"}'
`)
			ctx := t.Context()
			if tt.untrusted {
				ctx = WithUntrustedCheckout(ctx)
			}
			_, err := NewCursorAgent(command).Review(ctx, t.TempDir(), "HEAD", "prompt", nil)
			require.NoError(t, err)

			data, err := os.ReadFile(capture)
			require.NoError(t, err)
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if tt.untrusted {
				assert.NotEqual(t, userData, lines[0])
				assert.Len(t, lines, 1, "the data directory must start empty")
				assert.NoDirExists(t, lines[0], "the data directory is removed after the run")
			} else {
				assert.Equal(t, []string{userData, "trusted"}, lines)
			}
		})
	}
}
