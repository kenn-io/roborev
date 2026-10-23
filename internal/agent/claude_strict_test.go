package agent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

const (
	strictClaudeHelp   = "--restricted --tools --strict-mcp-config --mcp-config --permission-mode --permission-prompts --allowedTools --no-session-persistence --disable-slash-commands --effort --json-schema"
	strictClaudeInit   = `{"type":"system","subtype":"init","permissionMode":"dontAsk","tools":["Read","Glob","Grep"],"mcp_servers":[]}`
	strictClaudeResult = `{"type":"result","result":"{\"findings\":[]}"}`
)

func strictClaudeAgent(t *testing.T, command string) *ClaudeAgent {
	t.Helper()
	a := NewClaudeAgent(command)
	path := filepath.Join(t.TempDir(), "strict mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o600))
	return a.WithStrictReview(config.ReviewToolPolicy{Mode: "read-only"}, path).(*ClaudeAgent)
}

func callStrictClaudeReview(t *testing.T, a *ClaudeAgent, ctx context.Context, schema bool, prompt string) error {
	t.Helper()
	if schema {
		_, err := a.ReviewWithSchema(ctx, t.TempDir(), "HEAD", prompt, jsontext.Value(`{"type":"object"}`), nil)
		return err
	}
	_, err := a.Review(ctx, t.TempDir(), "HEAD", prompt, nil)
	return err
}

func TestClaudeStrictInvocation(t *testing.T) {
	for _, schema := range []bool{false, true} {
		t.Run(fmt.Sprintf("schema=%t", schema), func(t *testing.T) {
			withUnsafeAgents(t, true)
			mock := mockAgentCLI(t, MockCLIOpts{HelpOutput: strictClaudeHelp, CaptureStdin: true, StdoutLines: []string{strictClaudeInit, strictClaudeResult}})
			argsFile := filepath.Join(t.TempDir(), "args.bin")
			command := writeTempCommand(t, fmt.Sprintf("#!/bin/sh\nprintf '%%s\\000' \"$@\" > %q\nexec %q \"$@\"\n", argsFile, mock.CmdPath))
			a := strictClaudeAgent(t, command)
			mcpConfigPath := a.strictMCPConfigPath
			a = a.WithModel("review-model").WithReasoning(ReasoningThorough).(*ClaudeAgent).WithSessionID("old-session").(*ClaudeAgent)
			require.NoError(t, callStrictClaudeReview(t, a, context.Background(), schema, "review material"))
			captured, err := os.ReadFile(argsFile)
			require.NoError(t, err)
			args := strings.Split(strings.TrimSuffix(string(captured), "\x00"), "\x00")
			want := []string{"-p", "--verbose", "--output-format", "stream-json", "--restricted", "--tools", "Read,Glob,Grep", "--strict-mcp-config", "--mcp-config"}
			require.Greater(t, len(args), len(want))
			assert.Equal(t, want, args[:len(want)])
			want = append(want, mcpConfigPath, "--permission-mode", "dontAsk", "--permission-prompts", "none", "--allowedTools", "Read,Glob,Grep", "--no-session-persistence", "--disable-slash-commands", "--model", "review-model", "--effort", "high")
			assert.Equal(t, a.Command+" "+strings.Join(want, " "), a.CommandLine())
			if schema {
				want = append(want, "--json-schema", `{"type":"object"}`)
			}
			assert.Equal(t, want, args)
			data, err := os.ReadFile(mock.StdinFile)
			require.NoError(t, err)
			assert.Equal(t, "review material", string(data))
			assert.Empty(t, a.SessionID)
			assert.True(t, AllowUnsafeAgents())
		})
	}
}

func TestClaudeStrictStartup(t *testing.T) {
	for _, schema := range []bool{false, true} {
		for _, tc := range []struct {
			name, init string
			valid      bool
		}{
			{"valid", strictClaudeInit, true},
			{"missing", "", false},
			{"permission", strings.ReplaceAll(strictClaudeInit, "dontAsk", "bypassPermissions"), false},
			{"missing-read", strings.ReplaceAll(strictClaudeInit, `"Read",`, ""), false},
			{"shell", strings.ReplaceAll(strictClaudeInit, `"Grep"`, `"Grep","Bash"`), false},
			{"delegation", strings.ReplaceAll(strictClaudeInit, `"Grep"`, `"Grep","Agent"`), false},
			{"tool-search", strings.ReplaceAll(strictClaudeInit, `"Grep"`, `"Grep","ToolSearch"`), false},
			{"mcp-tool", strings.ReplaceAll(strictClaudeInit, `"Grep"`, `"Grep","mcp__archive__lookup"`), false},
			{"mcp-server", strings.ReplaceAll(strictClaudeInit, `"mcp_servers":[]`, `"mcp_servers":[{"name":"archive","status":"connected"}]`), false},
			{"malformed", `{"type":"system","subtype":"init","tools":false}`, false},
			{"structured-output", strings.ReplaceAll(strictClaudeInit, `"Grep"`, `"Grep","StructuredOutput"`), schema},
		} {
			t.Run(fmt.Sprintf("schema=%t/%s", schema, tc.name), func(t *testing.T) {
				mock := mockAgentCLI(t, MockCLIOpts{HelpOutput: strictClaudeHelp, StdoutLines: []string{tc.init, strictClaudeResult}})
				err := callStrictClaudeReview(t, strictClaudeAgent(t, mock.CmdPath), context.Background(), schema, "review")
				if tc.valid {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "strict Claude startup")
				}
			})
		}
	}
}

func TestClaudeStrictRequiredFlags(t *testing.T) {
	for _, schema := range []bool{false, true} {
		for flag := range strings.FieldsSeq(strictClaudeHelp) {
			// Effort is optional for ordinary calls, but a configured strict effort
			// must not be silently dropped. Schema is required only for schema calls.
			if flag == "--json-schema" && !schema {
				continue
			}
			t.Run(fmt.Sprintf("schema=%t/%s", schema, flag), func(t *testing.T) {
				help := strings.ReplaceAll(strictClaudeHelp, flag, flag+"-lookalike")
				mock := mockAgentCLI(t, MockCLIOpts{HelpOutput: help, CaptureArgs: true, StdoutLines: []string{strictClaudeInit, strictClaudeResult}})
				a := strictClaudeAgent(t, mock.CmdPath).WithReasoning(ReasoningHigh).(*ClaudeAgent)
				require.ErrorContains(t, callStrictClaudeReview(t, a, context.Background(), schema, "review"), "upgrade")
				_, err := os.Stat(mock.ArgsFile)
				require.ErrorIs(t, err, os.ErrNotExist, "capability failure must not launch a review")
			})
		}
	}
}

func TestClaudeStrictEnvironment(t *testing.T) {
	removed := []string{
		"CLAUDE_CODE_COORDINATOR_MODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_PID",
		"CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXECPATH",
		"CLAUDE_CODE_ENABLE_AWAY_SUMMARY", "CLAUDE_CODE_DISABLE_AUTO_MEMORY", "ENABLE_TOOL_SEARCH", "ENABLE_LSP_TOOL", "MAX_MCP_OUTPUT_TOKENS", "CLAUDE_EFFORT",
	}
	preserved := []string{
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_SKIP_BEDROCK_AUTH", "CLAUDE_CODE_SKIP_VERTEX_AUTH", "CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
		"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS", "CLAUDE_CODE_UNKNOWN",
	}
	for _, schema := range []bool{false, true} {
		for _, proxy := range []bool{false, true} {
			t.Run(fmt.Sprintf("schema=%t/proxy=%t", schema, proxy), func(t *testing.T) {
				for _, key := range append(append([]string{}, removed...), preserved...) {
					t.Setenv(key, "synthetic-value")
				}
				t.Setenv("ROBOREV_CLAUDE_PROXY_TOKEN", "synthetic-proxy")
				mock := mockAgentCLI(t, MockCLIOpts{HelpOutput: strictClaudeHelp, CaptureEnv: true, StdoutLines: []string{strictClaudeInit, strictClaudeResult}})
				// Capture only named synthetic variables, never the inherited environment.
				keys := append(append(append([]string{}, removed...), preserved...), "CLAUDE_CODE_SUBAGENT_MODEL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL")
				SetAnthropicAPIKey("synthetic-native")
				t.Cleanup(func() { SetAnthropicAPIKey("") })
				var capture strings.Builder
				fmt.Fprintf(&capture, ": > %q\n", mock.EnvFile)
				for _, key := range keys {
					fmt.Fprintf(&capture, "printf '%%s=%%s\\n' '%s' \"$%s\" >> %q\n", key, key, mock.EnvFile)
				}
				script, err := os.ReadFile(mock.CmdPath)
				require.NoError(t, err)
				script = []byte(strings.Replace(string(script), fmt.Sprintf("env > %q\n", mock.EnvFile), capture.String(), 1))
				require.NoError(t, os.WriteFile(mock.CmdPath, script, 0o700))

				model := "review-model"
				if proxy {
					model += "@http://127.0.0.1:1234"
				}
				a := strictClaudeAgent(t, mock.CmdPath).WithModel(model).(*ClaudeAgent)
				require.NoError(t, callStrictClaudeReview(t, a, context.Background(), schema, "review"))
				// Inspect only synthetic entries; diagnostics never dump inherited credentials.
				captured := readMockEnv(t, mock.EnvFile)
				for _, key := range removed {
					assert.NotContains(t, captured, key+"=synthetic-value")
					assert.Equal(t, "synthetic-value", os.Getenv(key))
				}
				for _, key := range preserved {
					assert.Contains(t, captured, key+"=synthetic-value")
				}
				if proxy {
					assert.Contains(t, captured, "CLAUDE_CODE_SUBAGENT_MODEL=review-model")
					assert.Contains(t, captured, "ANTHROPIC_AUTH_TOKEN=synthetic-proxy")
					assert.Contains(t, captured, "ANTHROPIC_API_KEY=")
				} else {
					assert.Contains(t, captured, "ANTHROPIC_API_KEY=synthetic-native")
				}
			})
		}
	}
}

func TestClaudeStrictStreamOrderingAndErrors(t *testing.T) {
	for _, schema := range []bool{false, true} {
		for _, tc := range []struct {
			name, stream string
			valid        bool
		}{
			{"late-init", strictClaudeResult + "\n" + strictClaudeInit, false},
			{"invalid-then-valid", strings.ReplaceAll(strictClaudeInit, "dontAsk", "default") + "\n" + strictClaudeInit + "\n" + strictClaudeResult, false},
			{"permission-denial", strictClaudeInit + "\n" + `{"type":"tool_result","is_error":true,"content":"Permission denied"}` + "\n" + strictClaudeResult, true},
			{"provider-error", strictClaudeInit + "\n" + `{"type":"result","is_error":true,"result":"{\"findings\":[]}"}`, false},
			{"missing-mcp-evidence", strings.ReplaceAll(strictClaudeInit, `,"mcp_servers":[]`, "") + "\n" + strictClaudeResult, false},
		} {
			t.Run(fmt.Sprintf("schema=%t/%s", schema, tc.name), func(t *testing.T) {
				_, err := parseClaudeStrictStream(strings.NewReader(tc.stream), nil, schema)
				if tc.valid {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		}
	}
}

func TestClaudeStrictEarlyProviderFailure(t *testing.T) {
	for _, schema := range []bool{false, true} {
		for _, source := range []string{"stderr-only", "stdout-error"} {
			for _, startup := range []string{"missing", "invalid", "valid"} {
				t.Run(fmt.Sprintf("schema=%t/%s/%s", schema, source, startup), func(t *testing.T) {
					const diagnostic = "quota exceeded"
					opts := MockCLIOpts{HelpOutput: strictClaudeHelp, ExitCode: 1}
					if startup != "missing" {
						init := strictClaudeInit
						if startup == "invalid" {
							init = strings.ReplaceAll(init, "dontAsk", "default")
						}
						opts.StdoutLines = append(opts.StdoutLines, init)
					}
					if source == "stderr-only" {
						opts.StderrLines = []string{diagnostic}
					} else {
						opts.StdoutLines = append(opts.StdoutLines, `{"type":"result","is_error":true,"result":"quota exceeded"}`)
					}
					mock := mockAgentCLI(t, opts)
					repoPath := t.TempDir()
					prepared, cleanup, err := PrepareReviewToolPolicy(NewClaudeAgent(mock.CmdPath), &config.ReviewToolPolicy{Mode: "read-only"}, nil, repoPath, false)
					require.NoError(t, err)
					t.Cleanup(cleanup)
					a := prepared.(*ClaudeAgent)
					var output string
					if schema {
						var raw jsontext.Value
						raw, err = a.ReviewWithSchema(t.Context(), repoPath, "HEAD", "review", jsontext.Value(`{"type":"object"}`), nil)
						output = string(raw)
					} else {
						output, err = a.Review(t.Context(), repoPath, "HEAD", "review", nil)
					}
					require.ErrorContains(t, err, diagnostic)
					var exitErr *exec.ExitError
					require.ErrorAs(t, err, &exitErr)
					assert := assert.New(t)
					assert.Equal(1, exitErr.ExitCode())
					assert.Equal(LimitKindQuota, ClassifyLimit(a.Name(), err.Error()).Kind)
					assert.Empty(output)
					if startup == "valid" {
						require.NotErrorIs(t, err, ErrReviewToolPolicy)
					} else {
						require.ErrorIs(t, err, ErrReviewToolPolicy)
					}
					cleanup()
					_, err = os.Stat(filepath.Dir(a.strictMCPConfigPath))
					require.ErrorIs(t, err, os.ErrNotExist)
				})
			}
		}
	}
}

func TestClaudeStrictPolicyClone(t *testing.T) {
	original := NewClaudeAgent("unused")
	original.SessionID = "old-session"
	policy := config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"archive": {"lookup"}}}
	strict := original.WithStrictReview(policy, "selected.json").(*ClaudeAgent)
	policy.MCP["archive"][0] = "write"
	clone := strict.WithReasoning(ReasoningHigh).WithModel("review-model").(*ClaudeAgent)
	strict.strictPolicy.MCP["archive"][0] = "other"
	assert.Equal(t, []string{"lookup"}, clone.strictPolicy.MCP["archive"])
	assert.Empty(t, clone.SessionID)
	assert.Equal(t, "old-session", original.SessionID)
	require.ErrorIs(t, callStrictClaudeReview(t, clone, t.Context(), false, "review"), ErrReviewToolPolicy, "nonempty grants cannot execute yet")
}

func FuzzClaudeStrictStartupTools(f *testing.F) {
	for _, tool := range []string{"Read", "Glob", "Grep", "StructuredOutput", "Bash", "mcp__archive__lookup", "", "ToolSearch"} {
		f.Add("dontAsk", tool, false)
		f.Add("dontAsk", tool, true)
	}
	f.Add("bypassPermissions", "Read", true)
	f.Fuzz(func(t *testing.T, mode, extra string, schema bool) {
		// JSON wire strings contain Unicode; invalid UTF-8 materializes as replacement characters.
		mode = strings.ToValidUTF8(mode, "\ufffd")
		extra = strings.ToValidUTF8(extra, "\ufffd")
		init, err := json.Marshal(map[string]any{"type": "system", "subtype": "init", "permissionMode": mode, "tools": []string{"Read", "Glob", "Grep", extra}, "mcp_servers": []any{}})
		require.NoError(t, err)
		result, err := parseClaudeStrictStream(strings.NewReader(string(init)+"\n"+strictClaudeResult), nil, schema)
		permitted := extra == "Read" || extra == "Glob" || extra == "Grep" || (schema && extra == "StructuredOutput")
		if mode == "dontAsk" && permitted {
			require.NoError(t, err)
			assert.JSONEq(t, `{"findings":[]}`, result)
		} else {
			require.ErrorIs(t, err, ErrReviewToolPolicy)
			assert.Empty(t, result)
		}
	})
}
