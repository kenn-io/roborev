package agent

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeOrdinaryCoordinatorEnv(t *testing.T) {
	for _, schema := range []bool{false, true} {
		for _, model := range []string{"sonnet", "review-model@http://127.0.0.1:1234"} {
			for _, mode := range []string{"ordinary", "agentic", "planning"} {
				t.Run(fmt.Sprintf("schema=%t/model=%s/%s", schema, model, mode), func(t *testing.T) {
					withUnsafeAgents(t, true)
					for key, value := range map[string]string{
						"CLAUDE_CODE_COORDINATOR_MODE": "synthetic-coordinator",
						"CLAUDE_CODE_SESSION_ID":       "synthetic-session",
						"CLAUDE_CODE_UNRELATED":        "preserved",
						"CLAUDE_CODE_USE_BEDROCK":      "synthetic-provider",
						"AWS_ACCESS_KEY_ID":            "synthetic-provider-key",
					} {
						t.Setenv(key, value)
					}
					mock := mockAgentCLI(t, MockCLIOpts{
						CaptureEnv:  true,
						CaptureArgs: true,
						HelpOutput:  "--dangerously-skip-permissions",
						StdoutLines: []string{`{"type":"result","result":"{\"findings\":[]}"}`},
					})
					a := NewClaudeAgent(mock.CmdPath).WithModel(model).WithAgentic(mode != "ordinary").(*ClaudeAgent)
					ctx := context.Background()
					if mode == "planning" {
						ctx = context.WithValue(ctx, planningContextKey{}, true)
					}
					require.NoError(t, callStrictClaudeReview(t, a, ctx, schema, "review"))
					// Only synthetic entries belong in assertion diagnostics.
					env := slices.DeleteFunc(readMockEnv(t, mock.EnvFile), func(entry string) bool {
						key, _, _ := strings.Cut(entry, "=")
						return !slices.Contains([]string{"CLAUDE_CODE_COORDINATOR_MODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_UNRELATED", "CLAUDE_CODE_USE_BEDROCK", "AWS_ACCESS_KEY_ID"}, key)
					})
					assert := assert.New(t)
					args := readMockArgs(t, mock.ArgsFile)
					if mode == "ordinary" {
						require.GreaterOrEqual(t, len(args), 3)
						assert.Equal([]string{"-p", "--settings", `{"env":{"CLAUDE_CODE_COORDINATOR_MODE":""}}`}, args[:3])
					} else {
						assert.NotContains(args, "--settings")
					}
					if mode != "ordinary" {
						assert.Contains(env, "CLAUDE_CODE_COORDINATOR_MODE=synthetic-coordinator")
					} else {
						assert.NotContains(env, "CLAUDE_CODE_COORDINATOR_MODE=synthetic-coordinator")
					}
					assert.Contains(env, "CLAUDE_CODE_SESSION_ID=synthetic-session")
					assert.Contains(env, "CLAUDE_CODE_UNRELATED=preserved")
					assert.Contains(env, "CLAUDE_CODE_USE_BEDROCK=synthetic-provider")
					assert.Contains(env, "AWS_ACCESS_KEY_ID=synthetic-provider-key")
					assert.Equal("synthetic-coordinator", os.Getenv("CLAUDE_CODE_COORDINATOR_MODE"))
				})
			}
		}
	}
}
