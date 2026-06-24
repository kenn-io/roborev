package agent

import (
	"bytes"
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolsArgValue returns the value of the --allowedTools argument.
func toolsArgValue(t *testing.T, args []string) string {
	t.Helper()
	idx := slices.Index(args, "--allowedTools")
	if idx != -1 && idx+1 < len(args) {
		return args[idx+1]
	}
	require.NotContains(t, args, "--allowedTools", "--allowedTools not found in args")
	return ""
}

func assertTools(t *testing.T, args []string, want []string, dontWant []string) {
	t.Helper()
	assert := assert.New(t)

	val := toolsArgValue(t, args)
	gotTools := strings.Split(val, ",")
	for i := range gotTools {
		gotTools[i] = strings.TrimSpace(gotTools[i])
	}

	// Check required
	for _, w := range want {
		assert.Contains(gotTools, w, "missing tool %q", w)
	}
	// Check forbidden
	for _, d := range dontWant {
		assert.NotContains(gotTools, d, "forbidden tool %q", d)
	}
}

func TestClaudeHelpProbeUsesExistingWorkingDirectory(t *testing.T) {
	skipIfWindows(t)

	cmdPath := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
if ! pwd >/dev/null 2>&1; then
  echo "getcwd: cannot access parent directories" >&2
  exit 1
fi
if [ "$1" = "--help" ]; then
  echo "--dangerously-skip-permissions"
  exit 0
fi
exit 1
`)

	originalWD, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, os.Chdir(originalWD))
	})

	deletedWD := t.TempDir()
	require.NoError(t, os.Chdir(deletedWD))
	require.NoError(t, os.RemoveAll(deletedWD))

	supported, err := claudeSupportsDangerousFlag(context.Background(), cmdPath)

	require.NoError(t, err)
	assert.True(t, supported)
}

func TestClaudeBuildArgs(t *testing.T) {
	a := NewClaudeAgent("claude")

	t.Run("ReviewMode", func(t *testing.T) {
		// Non-agentic mode (review only): read-only tools, no dangerous flag
		args := a.buildArgs(false, false)
		for _, req := range []string{"--output-format", "stream-json", "--verbose", "-p", "--allowedTools"} {
			assertContainsArg(t, args, req)
		}
		assertNotContainsArg(t, args, claudeDangerousFlag)

		assertTools(t, args,
			[]string{"Read", "Glob", "Grep"},
			[]string{"Edit", "Write", "Bash"})
	})

	t.Run("AgenticMode", func(t *testing.T) {
		// Agentic mode: write tools + dangerous flag
		args := a.buildArgs(true, false)
		assertContainsArg(t, args, claudeDangerousFlag)
		assertContainsArg(t, args, "--allowedTools")

		assertTools(t, args,
			[]string{"Edit", "Write", "Bash"},
			nil)
	})

	t.Run("ResumeSession", func(t *testing.T) {
		args := a.WithSessionID("session-123").(*ClaudeAgent).buildArgs(false, false)
		assertContainsArg(t, args, "--resume")
		assertContainsArg(t, args, "session-123")
	})

	t.Run("RejectInvalidResumeSession", func(t *testing.T) {
		args := a.WithSessionID("-bad-session").(*ClaudeAgent).buildArgs(false, false)
		assertNotContainsArg(t, args, "--resume")
		assertNotContainsArg(t, args, "-bad-session")
	})
}

func TestParseModel(t *testing.T) {
	valid := []struct {
		name        string
		spec        string
		wantModel   string
		wantBaseURL string
	}{
		{"PlainModel", "sonnet", "sonnet", ""},
		{"WithProxyHTTPLoopback", "glm-5.1:cloud@http://127.0.0.1:11434", "glm-5.1:cloud", "http://127.0.0.1:11434"},
		{"WithProxyHTTPLocalhost", "foo@http://localhost:4000", "foo", "http://localhost:4000"},
		{"WithProxyHTTPIPv6Loopback", "foo@http://[::1]:4000", "foo", "http://[::1]:4000"},
		{"WithProxyHTTPS", "foo@https://proxy.example.com", "foo", "https://proxy.example.com"},
		{"EmptyString", "", "", ""},
		{"NonURLSuffix", "vendor/model@v2", "vendor/model@v2", ""},
		{"ModelWithColon", "llama3.1:8b", "llama3.1:8b", ""},
		{"ModelWithAtAndProxy", "vendor/model@v2@http://127.0.0.1:11434", "vendor/model@v2", "http://127.0.0.1:11434"},
		// Paths may contain '@' by design — the URL is forwarded verbatim
		// to the proxy, which is responsible for routing. parseModel splits
		// at the first '@' whose suffix starts with http(s):// and treats
		// everything after as the URL.
		{"ProxyURLPathContainsAt", "foo@http://127.0.0.1:11434/path@extra", "foo", "http://127.0.0.1:11434/path@extra"},
		// A second embedded http(s):// in the path portion is also forwarded
		// verbatim — we split at the first '@' whose suffix is a URL and
		// trust the proxy to route the rest. Locks in intended behavior.
		{"ProxyURLPathContainsSecondURL", "foo@http://127.0.0.1:11434/p@http://x", "foo", "http://127.0.0.1:11434/p@http://x"},
		// Query strings are forwarded verbatim to the proxy (README promise).
		{"ProxyURLWithQuery", "foo@https://proxy.example.com/v1?key=x", "foo", "https://proxy.example.com/v1?key=x"},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			m, u, err := parseModel(tt.spec)
			require.NoError(t, err)
			assert.Equal(tt.wantModel, m)
			assert.Equal(tt.wantBaseURL, u)
		})
	}

	invalid := []struct {
		name      string
		spec      string
		errSubstr string
	}{
		{"ProxyOnlyNoModel", "@http://localhost:4000", "no model"},
		{"ProxyOnlyNoModelHTTPS", "@https://proxy.example.com", "no model"},
		{"TrailingAt", "glm-5.1:cloud@", "trailing '@'"},
		{"TrailingAtMultiple", "foo@@", "trailing '@'"},
		{"ProxyURLWithUserinfo", "foo@http://user:pass@proxy.example.com", "must not embed credentials"},
		{"ProxyURLWithUserOnly", "foo@https://bob@proxy.example.com", "must not embed credentials"},
		{"HTTPNonLoopback", "foo@http://proxy.example.com", "non-loopback"},
		{"HTTPPublicIP", "foo@http://8.8.8.8:4000", "non-loopback"},
		{"BareHTTPURL", "http://127.0.0.1:11434", "bare proxy URL"},
		{"BareHTTPSURL", "https://proxy.example.com", "bare proxy URL"},
		{"LeadingAtNonURL", "@foo", "starts with '@'"},
		{"MalformedMultiURL", "foo@httpx://bar@http://127.0.0.1:11434", "URL-like substring"},
		{"ProxyURLWithFragment", "foo@http://127.0.0.1:11434/#frag", "fragment"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			_, _, err := parseModel(tt.spec)
			require.Error(t, err)
			assert.Contains(err.Error(), tt.errSubstr)
		})
	}
}

func TestClaudeBuildArgsStripsProxySuffix(t *testing.T) {
	a := NewClaudeAgent("claude").WithModel("glm-5.1:cloud@http://127.0.0.1:11434").(*ClaudeAgent)
	args := a.buildArgs(false, false)

	idx := slices.Index(args, "--model")
	require.NotEqual(t, -1, idx)
	require.Less(t, idx+1, len(args))
	assert.Equal(t, "glm-5.1:cloud", args[idx+1])

	for _, a := range args {
		assert.NotContains(t, a, "@")
		assert.NotContains(t, a, "http://")
	}
}

func TestClaudeBuildArgsFallsBackToRawModelOnParseError(t *testing.T) {
	// CommandLine() is used for logs/display. When the spec is malformed
	// (e.g. trailing '@'), buildArgs should still surface the raw configured
	// model so operators can see what they typed; Review() rejects the spec
	// before execution.
	a := NewClaudeAgent("claude").WithModel("sonnet@").(*ClaudeAgent)
	args := a.buildArgs(false, false)

	idx := slices.Index(args, "--model")
	require.NotEqual(t, -1, idx, "--model should be present with raw model fallback")
	require.Less(t, idx+1, len(args))
	assert.Equal(t, "sonnet@", args[idx+1])
}

func TestClaudeReviewProxyEnv(t *testing.T) {
	// Clear host env and package state so the proxy auth-token assertion is
	// deterministic regardless of the operator's shell or prior tests.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ROBOREV_CLAUDE_PROXY_TOKEN", "")
	SetAnthropicAPIKey("")
	t.Cleanup(func() { SetAnthropicAPIKey("") })

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureEnv: true,
		StdoutLines: []string{
			`{"type":"result","result":"ok"}`,
		},
	})

	a := NewClaudeAgent(mock.CmdPath).WithModel("glm-5.1:cloud@http://127.0.0.1:11434").(*ClaudeAgent)
	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	env := readMockEnv(t, mock.EnvFile)
	assert := assert.New(t)
	assert.Contains(env, "ANTHROPIC_BASE_URL=http://127.0.0.1:11434")
	assert.Contains(env, "ANTHROPIC_AUTH_TOKEN=proxy")
	assert.Contains(env, "ANTHROPIC_DEFAULT_OPUS_MODEL=glm-5.1:cloud")
	assert.Contains(env, "ANTHROPIC_DEFAULT_SONNET_MODEL=glm-5.1:cloud")
	assert.Contains(env, "ANTHROPIC_DEFAULT_HAIKU_MODEL=glm-5.1:cloud")
	assert.Contains(env, "CLAUDE_CODE_SUBAGENT_MODEL=glm-5.1:cloud")
	// No ANTHROPIC_API_KEY in proxy mode
	for _, e := range env {
		assert.False(strings.HasPrefix(e, "ANTHROPIC_API_KEY="), "unexpected ANTHROPIC_API_KEY in proxy mode: %s", e)
	}
}

func TestClaudeReviewProxyTokenOptIn(t *testing.T) {
	// ROBOREV_CLAUDE_PROXY_TOKEN is the only way to forward a real bearer
	// token to a proxy. Verifies the strip-then-inject ordering in Review()
	// doesn't accidentally reintroduce ANTHROPIC_API_KEY as
	// ANTHROPIC_AUTH_TOKEN — defense-in-depth against a future refactor
	// that might try to reuse the configured Anthropic key.
	t.Setenv("ANTHROPIC_API_KEY", "sk-real-anthropic-key")
	SetAnthropicAPIKey("sk-real-anthropic-key")
	t.Setenv("ROBOREV_CLAUDE_PROXY_TOKEN", "sk-proxy-only")
	t.Cleanup(func() { SetAnthropicAPIKey("") })

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureEnv: true,
		StdoutLines: []string{
			`{"type":"result","result":"ok"}`,
		},
	})

	a := NewClaudeAgent(mock.CmdPath).WithModel("glm-5.1:cloud@https://proxy.example.com").(*ClaudeAgent)
	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	env := readMockEnv(t, mock.EnvFile)
	assert := assert.New(t)
	assert.Contains(env, "ANTHROPIC_AUTH_TOKEN=sk-proxy-only")
	for _, e := range env {
		assert.NotEqual("ANTHROPIC_AUTH_TOKEN=sk-real-anthropic-key", e, "ANTHROPIC_API_KEY must not be forwarded as proxy auth token")
		assert.False(strings.HasPrefix(e, "ANTHROPIC_API_KEY="), "ANTHROPIC_API_KEY must not be set in proxy mode")
	}
}

func TestClaudeReviewProxyDoesNotForwardAPIKey(t *testing.T) {
	// Even when a real ANTHROPIC_API_KEY is configured and no explicit proxy
	// token is set, roborev must NOT fall back to forwarding the API key.
	// It should use the "proxy" placeholder instead.
	t.Setenv("ROBOREV_CLAUDE_PROXY_TOKEN", "")
	SetAnthropicAPIKey("sk-real-anthropic-key")
	t.Cleanup(func() { SetAnthropicAPIKey("") })

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureEnv: true,
		StdoutLines: []string{
			`{"type":"result","result":"ok"}`,
		},
	})

	a := NewClaudeAgent(mock.CmdPath).WithModel("glm-5.1:cloud@http://127.0.0.1:11434").(*ClaudeAgent)
	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	env := readMockEnv(t, mock.EnvFile)
	assert := assert.New(t)
	assert.Contains(env, "ANTHROPIC_AUTH_TOKEN=proxy")
	for _, e := range env {
		assert.NotContains(e, "sk-real-anthropic-key", "real API key leaked into proxy env: %s", e)
	}
}

func TestClaudeReviewStripsInheritedProxyEnv(t *testing.T) {
	// If the user has ANTHROPIC_BASE_URL or tier-alias vars exported in their
	// shell, native-mode Claude must not inherit them — otherwise roborev
	// silently routes through someone else's proxy. Covers every key in
	// the strip list so a future refactor that drops one trips this test.
	t.Setenv("ANTHROPIC_API_KEY", "stale-api-key")
	SetAnthropicAPIKey("")
	t.Cleanup(func() { SetAnthropicAPIKey("") })
	t.Setenv("ANTHROPIC_BASE_URL", "http://stale.example")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "stale-token")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "stale-opus")
	t.Setenv("ANTHROPIC_DEFAULT_SONNET_MODEL", "stale-sonnet")
	t.Setenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", "stale-haiku")
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "stale-subagent")
	t.Setenv("CLAUDECODE", "1")

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureEnv: true,
		StdoutLines: []string{
			`{"type":"result","result":"ok"}`,
		},
	})

	a := NewClaudeAgent(mock.CmdPath).WithModel("sonnet").(*ClaudeAgent)
	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	env := readMockEnv(t, mock.EnvFile)
	assert := assert.New(t)
	leakyPrefixes := []string{
		"ANTHROPIC_API_KEY=",
		"ANTHROPIC_BASE_URL=",
		"ANTHROPIC_AUTH_TOKEN=",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=",
		"ANTHROPIC_DEFAULT_SONNET_MODEL=",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=",
		"CLAUDE_CODE_SUBAGENT_MODEL=",
		"CLAUDECODE=",
	}
	for _, e := range env {
		for _, p := range leakyPrefixes {
			assert.False(strings.HasPrefix(e, p), "inherited %s leaked: %s", p, e)
		}
	}
}

func TestClaudeReviewProxyModeOverridesInheritedEnv(t *testing.T) {
	// Even if the parent env has stale proxy vars, proxy mode must end up
	// with exactly one entry per key, set to roborev's values.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ROBOREV_CLAUDE_PROXY_TOKEN", "")
	SetAnthropicAPIKey("")
	t.Cleanup(func() { SetAnthropicAPIKey("") })
	t.Setenv("ANTHROPIC_BASE_URL", "http://stale.example")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "stale-opus")
	t.Setenv("ANTHROPIC_DEFAULT_SONNET_MODEL", "stale-sonnet")
	t.Setenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", "stale-haiku")

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureEnv: true,
		StdoutLines: []string{
			`{"type":"result","result":"ok"}`,
		},
	})

	a := NewClaudeAgent(mock.CmdPath).WithModel("glm-5.1:cloud@http://127.0.0.1:11434").(*ClaudeAgent)
	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	env := readMockEnv(t, mock.EnvFile)
	assert := assert.New(t)
	countBaseURL, countSonnet := 0, 0
	for _, e := range env {
		if strings.HasPrefix(e, "ANTHROPIC_BASE_URL=") {
			countBaseURL++
			assert.Equal("ANTHROPIC_BASE_URL=http://127.0.0.1:11434", e)
		}
		if strings.HasPrefix(e, "ANTHROPIC_DEFAULT_SONNET_MODEL=") {
			countSonnet++
			assert.Equal("ANTHROPIC_DEFAULT_SONNET_MODEL=glm-5.1:cloud", e)
		}
	}
	assert.Equal(1, countBaseURL, "expected exactly one ANTHROPIC_BASE_URL")
	assert.Equal(1, countSonnet, "expected exactly one ANTHROPIC_DEFAULT_SONNET_MODEL")
}

func TestClaudeReviewProxyTokenRejectsControlChars(t *testing.T) {
	// Tokens containing embedded control characters would either be rejected
	// by exec (NUL) or produce malformed HTTP headers. Fail fast with a clear
	// error rather than passing them through to the proxy.
	SetAnthropicAPIKey("")
	t.Cleanup(func() { SetAnthropicAPIKey("") })

	// NUL is rejected by os.Setenv itself so it's unreachable via the env-var
	// path, but the validation defends against it for completeness.
	tests := []struct {
		name  string
		token string
	}{
		{"EmbeddedNewline", "sk-proxy\nbad"},
		{"EmbeddedCarriageReturn", "sk-proxy\rbad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ROBOREV_CLAUDE_PROXY_TOKEN", tt.token)
			a := NewClaudeAgent("claude").WithModel("glm-5.1:cloud@http://127.0.0.1:11434").(*ClaudeAgent)
			_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "control characters")
		})
	}
}

func TestClaudeReviewProxyOnlyRejected(t *testing.T) {
	a := NewClaudeAgent("claude").WithModel("@http://localhost:4000").(*ClaudeAgent)
	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no model")
}

func TestClaudeReviewRejectsInvalidSpecs(t *testing.T) {
	// Review must surface parser errors so malformed specs fail fast instead
	// of silently falling back to native routing or leaking credentials.
	tests := []struct {
		name      string
		model     string
		errSubstr string
	}{
		{"TrailingAt", "sonnet@", "trailing '@'"},
		{"UserinfoInURL", "foo@http://user:pass@proxy.example.com", "must not embed credentials"},
		{"HTTPNonLoopback", "foo@http://proxy.example.com", "non-loopback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewClaudeAgent("claude").WithModel(tt.model).(*ClaudeAgent)
			_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errSubstr)
		})
	}
}

func TestClaudeReviewNonProxyDoesNotSetProxyEnv(t *testing.T) {
	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureEnv: true,
		StdoutLines: []string{
			`{"type":"result","result":"ok"}`,
		},
	})

	a := NewClaudeAgent(mock.CmdPath).WithModel("sonnet").(*ClaudeAgent)
	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	env := readMockEnv(t, mock.EnvFile)
	assert := assert.New(t)
	for _, e := range env {
		assert.False(strings.HasPrefix(e, "ANTHROPIC_BASE_URL="), "unexpected ANTHROPIC_BASE_URL: %s", e)
		assert.False(strings.HasPrefix(e, "ANTHROPIC_AUTH_TOKEN="), "unexpected ANTHROPIC_AUTH_TOKEN: %s", e)
		assert.False(strings.HasPrefix(e, "ANTHROPIC_DEFAULT_SONNET_MODEL="), "unexpected ANTHROPIC_DEFAULT_SONNET_MODEL: %s", e)
	}
}

func TestClaudeDangerousFlagSupport(t *testing.T) {
	assert := assert.New(t)

	cmdPath := writeTempCommand(t, "#!/bin/sh\necho \"usage "+claudeDangerousFlag+"\"; exit 1\n")

	supported, err := claudeSupportsDangerousFlag(context.Background(), cmdPath)
	require.NoError(t, err)
	assert.True(supported, "expected dangerous flag support")
}

func TestClaudeReviewUnsafeMissingFlagErrors(t *testing.T) {
	assert := assert.New(t)

	withUnsafeAgents(t, true)

	cmdPath := writeTempCommand(t, "#!/bin/sh\nif [ \"$1\" = \"--help\" ]; then echo \"usage\"; exit 0; fi\nexit 0\n")

	a := NewClaudeAgent(cmdPath)
	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.Error(t, err)
	assert.Contains(err.Error(), "does not support")
}

func TestClaudeReviewWithSessionResumePassesResumeArgs(t *testing.T) {
	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureArgs: true,
		StdoutLines: []string{
			`{"type":"result","result":"ok"}`,
		},
	})

	a := NewClaudeAgent(mock.CmdPath).WithSessionID("session-123").(*ClaudeAgent)
	_, err := a.Review(context.Background(), t.TempDir(), "deadbeef", "prompt", nil)
	require.NoError(t, err)

	args := readMockArgs(t, mock.ArgsFile)
	assertContainsArg(t, args, "--resume")
	assertContainsArg(t, args, "session-123")
}

func TestParseStreamJSON(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		expectedResult string
		expectedErr    string // substring match
		expectOutput   bool   // verify output buffer was written to
	}{
		{
			name: "ResultEvent",
			input: `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":"Working on it..."}}
{"type":"result","result":"Done! Created the file."}
`,
			expectedResult: "Done! Created the file.",
		},
		{
			name: "AssistantFallback",
			input: `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":"First message"}}
{"type":"assistant","message":{"content":"Second message"}}
`,
			expectedResult: "First message\nSecond message",
		},
		{
			name: "MalformedLines",
			input: `{"type":"system","subtype":"init"}
not valid json
{"type":"result","result":"Success"}
also not json
`,
			expectedResult: "Success",
		},
		{
			name: "NoValidEvents",
			input: `not json at all
still not json
`,
			expectedErr: "no valid stream-json events",
		},
		{
			name:        "EmptyInput",
			input:       "",
			expectedErr: "no valid stream-json events",
		},
		{
			name: "StreamsToOutput",
			input: `{"type":"system","subtype":"init"}
{"type":"result","result":"Done"}
`,
			expectedResult: "Done",
			expectOutput:   true,
		},
		// Error detection: is_error flag on result events
		{
			name: "ErrorResultWithErrorMessage",
			input: `{"type":"system","subtype":"init"}
{"type":"result","is_error":true,"result":"","error":{"message":"Invalid API key"}}
`,
			expectedErr: "Invalid API key",
		},
		{
			name: "ErrorResultWithResultText",
			input: `{"type":"system","subtype":"init"}
{"type":"result","is_error":true,"result":"Your credit balance is too low"}
`,
			expectedErr: "Your credit balance is too low",
		},
		{
			name: "ErrorResultNoDetails",
			input: `{"type":"system","subtype":"init"}
{"type":"result","is_error":true,"result":""}
`,
			expectedErr: "review returned error",
		},
		{
			name: "ErrorResultWithPartialAssistant",
			input: `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":"I'll review this commit..."}}
{"type":"result","is_error":true,"result":"","error":{"message":"rate limit exceeded"}}
`,
			expectedErr: "rate limit exceeded",
		},
		// Content array format (real Claude Code output)
		{
			name: "ContentBlockArray",
			input: `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Reviewing the changes..."}]}}
{"type":"result","result":"All good."}
`,
			expectedResult: "All good.",
		},
		{
			name: "ContentBlockArrayFallback",
			input: `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"First block"},{"type":"text","text":"Second block"}]}}
`,
			expectedResult: "First block\nSecond block",
		},
		{
			name: "AssistantFallbackDropsNarrationBeforeToolUse",
			input: `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":"I am checking the relevant files first."}}
{"type":"tool_use","name":"Read","input":{"file_path":"redacted.go"}}
{"type":"tool_result","content":"redacted"}
{"type":"assistant","message":{"content":"## Review Findings\n- **Severity**: Low; **Problem**: Final finding."}}
`,
			expectedResult: "## Review Findings\n- **Severity**: Low; **Problem**: Final finding.",
		},
		{
			name: "AssistantFallbackPrefersFinalPostToolSegment",
			input: `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":"## Review Findings\n- **Severity**: Low; **Problem**: Earlier provisional finding."}}
{"type":"tool_use","name":"Read","input":{"file_path":"redacted.go"}}
{"type":"tool_result","content":"redacted"}
{"type":"assistant","message":{"content":"## Review Findings\n- **Severity**: Medium; **Problem**: Final persisted finding."}}
`,
			expectedResult: "## Review Findings\n- **Severity**: Medium; **Problem**: Final persisted finding.",
		},
		{
			name: "ContentBlockArrayWithToolUse",
			input: `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Let me check..."},{"type":"tool_use","name":"Read"}]}}
{"type":"result","result":"Found the issue."}
`,
			expectedResult: "Found the issue.",
		},
		// Standalone error events (non-result)
		{
			name: "StandaloneErrorEvent",
			input: `{"type":"system","subtype":"init"}
{"type":"error","error":{"message":"connection reset"}}
`,
			expectedErr: "connection reset",
		},
		// Empty output (valid events, no content)
		{
			name:           "ValidEventsNoContent",
			input:          `{"type":"system","subtype":"init"}` + "\n",
			expectedResult: "",
		},
		// Non-error result with is_error=false (normal path)
		{
			name: "ResultIsErrorFalse",
			input: `{"type":"system","subtype":"init"}
{"type":"result","is_error":false,"result":"No issues found."}
`,
			expectedResult: "No issues found.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			var out bytes.Buffer
			var w io.Writer
			if tt.expectOutput {
				w = &out
			}

			res, err := parseStreamJSON(strings.NewReader(tt.input), w)

			if tt.expectedErr != "" {
				require.Error(err)
				assert.Contains(err.Error(), tt.expectedErr)
				return
			}

			require.NoError(err)
			assert.Equal(tt.expectedResult, res)

			if tt.expectOutput {
				assert.NotZero(out.Len(), "expected output to be written")
			}
		})
	}
}

func TestExtractContentText(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "PlainString",
			raw:  `"Hello world"`,
			want: "Hello world",
		},
		{
			name: "SingleTextBlock",
			raw:  `[{"type":"text","text":"Review complete."}]`,
			want: "Review complete.",
		},
		{
			name: "MultipleTextBlocks",
			raw:  `[{"type":"text","text":"First"},{"type":"text","text":"Second"}]`,
			want: "First\nSecond",
		},
		{
			name: "MixedBlockTypes",
			raw:  `[{"type":"text","text":"Analysis"},{"type":"tool_use","name":"Read"},{"type":"text","text":"Done"}]`,
			want: "Done",
		},
		{
			name: "EmptyArray",
			raw:  `[]`,
			want: "",
		},
		{
			name: "NilInput",
			raw:  "",
			want: "",
		},
		{
			name: "EmptyString",
			raw:  `""`,
			want: "",
		},
		{
			name: "EmptyTextBlock",
			raw:  `[{"type":"text","text":""}]`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)

			got := extractContentText([]byte(tt.raw))
			assert.Equal(tt.want, got)
		})
	}
}

func TestClaudeReviewEmptyOutput(t *testing.T) {
	assert := assert.New(t)

	// When Claude produces valid events but no review content,
	// Review() should return "No review output generated" (not empty).
	script := `#!/bin/sh
if [ "$1" = "--help" ]; then echo "usage"; exit 0; fi
echo '{"type":"system","subtype":"init"}'
exit 0
`
	cmdPath := writeTempCommand(t, script)

	a := NewClaudeAgent(cmdPath)
	result, err := a.Review(
		context.Background(), t.TempDir(), "abc123", "review this", nil,
	)
	require.NoError(t, err)
	assert.Equal("No review output generated", result)
}

func TestClaudeReviewStreamsProgressToOutput(t *testing.T) {
	assert := assert.New(t)

	script := `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
if [ "$1" = "--help" ]; then echo "usage"; exit 0; fi
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Reviewing..."}]}}'
echo '{"type":"result","result":"LGTM"}'
exit 0
`
	cmdPath := writeTempCommand(t, script)

	a := NewClaudeAgent(cmdPath)
	var output strings.Builder
	result, err := a.Review(
		context.Background(), t.TempDir(), "abc123", "review this", &output,
	)
	require.NoError(t, err)
	assert.Equal("LGTM", result)
	assert.Contains(output.String(), `"type":"assistant"`)
	assert.Contains(output.String(), `"Reviewing..."`)
}

func TestClaudeReviewErrorResult(t *testing.T) {
	assert := assert.New(t)

	// When Claude exits with code 0 but emits is_error result,
	// the error should propagate as a failure.
	script := `#!/bin/sh
if [ "$1" = "--help" ]; then echo "usage"; exit 0; fi
echo '{"type":"system","subtype":"init"}'
echo '{"type":"result","is_error":true,"result":"","error":{"message":"Invalid API key"}}'
exit 0
`
	cmdPath := writeTempCommand(t, script)

	a := NewClaudeAgent(cmdPath)
	_, err := a.Review(
		context.Background(), t.TempDir(), "abc123", "review this", nil,
	)
	require.Error(t, err)
	assert.Contains(err.Error(), "Invalid API key")
}

func TestClaudeReviewErrorResultNonZeroExit(t *testing.T) {
	assert := assert.New(t)

	// When Claude emits is_error result AND exits non-zero,
	// the error should include both stream error and exit status.
	script := `#!/bin/sh
if [ "$1" = "--help" ]; then echo "usage"; exit 0; fi
echo '{"type":"system","subtype":"init"}'
echo '{"type":"result","is_error":true,"result":"","error":{"message":"quota exceeded"}}'
exit 1
`
	cmdPath := writeTempCommand(t, script)

	a := NewClaudeAgent(cmdPath)
	_, err := a.Review(
		context.Background(), t.TempDir(), "abc123", "review this", nil,
	)
	require.Error(t, err)
	errStr := err.Error()
	assert.Contains(errStr, "quota exceeded")
	assert.Contains(errStr, "failed")
}

func TestAnthropicAPIKey(t *testing.T) {
	assert := assert.New(t)

	// Clear any existing key
	SetAnthropicAPIKey("")
	t.Cleanup(func() { SetAnthropicAPIKey("") })

	// Initially should be empty
	assert.Empty(AnthropicAPIKey())

	// Set a key
	SetAnthropicAPIKey("test-api-key")
	assert.Equal("test-api-key", AnthropicAPIKey())

	// Clear the key
	SetAnthropicAPIKey("")
	assert.Empty(AnthropicAPIKey())
}

func TestFilterEnv(t *testing.T) {
	tests := []struct {
		name     string
		env      []string
		keys     []string
		expected []string
	}{
		{
			name:     "SingleKey",
			env:      []string{"PATH=/bin", "KEY=secret", "OTHER=val"},
			keys:     []string{"KEY"},
			expected: []string{"PATH=/bin", "OTHER=val"},
		},
		{
			name:     "MultipleKeys",
			env:      []string{"PATH=/bin", "KEY1=s1", "KEY2=s2", "HOME=/home"},
			keys:     []string{"KEY1", "KEY2"},
			expected: []string{"PATH=/bin", "HOME=/home"},
		},
		{
			name:     "ExactMatchOnly",
			env:      []string{"PRE_KEY=1", "KEY=1", "KEY_POST=1"},
			keys:     []string{"KEY"},
			expected: []string{"PRE_KEY=1", "KEY_POST=1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)

			got := filterEnv(tt.env, tt.keys...)
			assert.Equal(tt.expected, got)
		})
	}
}

func TestClaudeClassify_BuildsArgs(t *testing.T) {
	a := NewClaudeAgent("claude")
	got := a.classifyArgs([]byte(`{"type":"object"}`))
	assert := assert.New(t)
	assert.Contains(got, "-p")
	assert.Contains(got, "--json-schema")
	idx := -1
	for i, arg := range got {
		if arg == "--json-schema" {
			idx = i
			break
		}
	}
	require.NotEqual(t, -1, idx)
	require.Less(t, idx+1, len(got))
	assert.JSONEq(`{"type":"object"}`, got[idx+1])
	// Security: classify must disable ALL tools (so prompt-injected
	// commits cannot read ~/.ssh, ~/.roborev, etc. and stuff secrets
	// into the JSON reason field). Never pass --dangerously-skip-permissions.
	assert.NotContains(got, "--dangerously-skip-permissions")
	assert.NotContains(got, "--allowedTools")
	idx = -1
	for i, arg := range got {
		if arg == "--tools" {
			idx = i
			break
		}
	}
	require.NotEqual(t, -1, idx)
	require.Less(t, idx+1, len(got))
	assert.Empty(got[idx+1], "classify must pass --tools \"\" to disable file/tool access")
}

func TestClaudeClassify_ParseResult_OldResultEvent(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"partial"}]}}
{"type":"result","result":"{\"design_review\":true,\"reason\":\"new package\"}"}
`
	out, err := parseClaudeClassifyStream(strings.NewReader(stream))
	require.NoError(t, err)
	assert.JSONEq(t, `{"design_review":true,"reason":"new package"}`, string(out))
}

func TestClaudeClassify_ParseResult_StructuredOutputToolUse(t *testing.T) {
	stream := `{"type":"system","subtype":"init","tools":["StructuredOutput"]}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"StructuredOutput","input":{"design_review":true,"reason":"new public endpoint"},"caller":{"type":"direct"}}]}}
{"type":"result","subtype":"success","stop_reason":"tool_use"}
`
	out, err := parseClaudeClassifyStream(strings.NewReader(stream))
	require.NoError(t, err)
	assert.JSONEq(t, `{"design_review":true,"reason":"new public endpoint"}`, string(out))
}

func TestClaudeClassify_ParseResult_AssistantProseFails(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"{\"design_review\":true,\"reason\":\"prose\"}"}]}}
`
	_, err := parseClaudeClassifyStream(strings.NewReader(stream))
	assert.ErrorContains(t, err, "no result or structured output event")
}

func TestClaudeClassify_ParseResult_InvalidJSON(t *testing.T) {
	stream := `{"type":"result","result":"not valid json"}` + "\n"
	_, err := parseClaudeClassifyStream(strings.NewReader(stream))
	assert.ErrorContains(t, err, "claude result is not valid JSON")
}

func TestClaudeClassify_ParseResult_InvalidStructuredOutput(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"StructuredOutput","input":"not valid classifier JSON","caller":{"type":"direct"}}]}}` + "\n"
	_, err := parseClaudeClassifyStream(strings.NewReader(stream))
	assert.ErrorContains(t, err, "claude structured output is not a JSON object")
}
