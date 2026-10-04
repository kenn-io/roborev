package tokens

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSchemaUsagePiSumsCompletedAssistantMessages(t *testing.T) {
	stream := `progress before JSON
{"type":"session","id":"session-a"}
{"type":"message_end","message":{"role":"user","usage":{"input":900}}}
{"type":"message_end","message":{"role":"assistant","usage":{"input":100,"output":10,"cacheRead":20,"cacheWrite":30,"cost":{"total":0.25}}}}
{"type":"message_end","message":{"role":"assistant","usage":{"input":200,"output":15,"cacheRead":40,"cacheWrite":50,"cost":{"total":0.5}},"content":[{"type":"text","text":"` + strings.Repeat("x", 70_000) + `"}]}}
{"type":"agent_end","messages":[{"role":"assistant","usage":{"input":200,"output":15,"cost":{"total":0.5}}}]}`
	usage, err := ParseSchemaUsage("pi", strings.NewReader(stream))
	require.NoError(t, err)
	assert.Equal(t, &Usage{
		InputTokens: 300, OutputTokens: 25, CachedInputTokens: 60, CacheCreationTokens: 80,
		CostUSD: 0.75, HasCost: true, ThreadID: "session-a", UsageSource: "job_log_pi_schema",
	}, usage)
}

func TestParseSchemaUsageClaudeUsesFinalCumulativeResult(t *testing.T) {
	stream := `{"type":"system","session_id":"session-b"}
{"type":"assistant","message":{"usage":{"input_tokens":900,"output_tokens":900}}}
{"type":"result","usage":{"input_tokens":100,"output_tokens":10},"total_cost_usd":0.25}
{"type":"result","usage":{"input_tokens":300,"output_tokens":25,"cache_read_input_tokens":60,"cache_creation_input_tokens":80},"total_cost_usd":0.75}`
	usage, err := ParseSchemaUsage("claude-code", strings.NewReader(stream))
	require.NoError(t, err)
	assert.Equal(t, &Usage{
		InputTokens: 300, OutputTokens: 25, CachedInputTokens: 60, CacheCreationTokens: 80,
		CostUSD: 0.75, HasCost: true, ThreadID: "session-b", UsageSource: "job_log_claude_schema",
	}, usage)
}

func TestParseSchemaUsagePreservesUnknownCost(t *testing.T) {
	for _, tc := range []struct {
		name, agent, stream string
		priced              bool
	}{
		{"Pi missing", "pi", `{"type":"message_end","message":{"role":"assistant","usage":{"output":1}}}`, false},
		{"Pi null", "pi", `{"type":"message_end","message":{"role":"assistant","usage":{"output":1,"cost":{"total":null}}}}`, false},
		{"Pi free", "pi", `{"type":"message_end","message":{"role":"assistant","usage":{"output":1,"cost":{"total":0}}}}`, true},
		{"Pi incomplete", "pi", "{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"usage\":{\"output\":1,\"cost\":{\"total\":0}}}}\n{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input\":1}}}", false},
		{"Claude missing", "claude-code", `{"type":"result","usage":{"output_tokens":1}}`, false},
		{"Claude null", "claude-code", `{"type":"result","usage":{"output_tokens":1},"total_cost_usd":null}`, false},
		{"Claude free", "claude-code", `{"type":"result","usage":{"output_tokens":1},"total_cost_usd":0}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage, err := ParseSchemaUsage(tc.agent, strings.NewReader(tc.stream))
			require.NoError(t, err)
			require.NotNil(t, usage)
			assert.Equal(t, int64(1), usage.OutputTokens)
			assert.Equal(t, tc.priced, usage.HasCost)
		})
	}
	for _, agent := range []string{"pi", "claude-code"} {
		usage, err := ParseSchemaUsage(agent, strings.NewReader("progress only\n{}\n"))
		require.NoError(t, err)
		assert.Nil(t, usage)
	}
}

func TestParseSchemaUsageReportsReadFailure(t *testing.T) {
	want := errors.New("read interrupted")
	usage, err := ParseSchemaUsage("pi", iotest.ErrReader(want))
	require.ErrorIs(t, err, want)
	assert.Nil(t, usage)
}
