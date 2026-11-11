package tokens

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// claudeResult builds a Claude result event. Each models entry is
// {input, output, cache read, cache creation} with a fixed cost of $0.10.
func claudeResult(sessionID, field string, models map[string][4]int64) string {
	var entries []string
	for name, counts := range models {
		entries = append(entries, fmt.Sprintf(
			`%q:{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":%d,`+
				`"cacheCreationInputTokens":%d,"costUSD":0.1}`,
			name, counts[0], counts[1], counts[2], counts[3]))
	}
	return fmt.Sprintf(`{"type":"result","session_id":%q,%q:{%s}}`,
		sessionID, field, strings.Join(entries, ","))
}

func TestParseSchemaUsageClaudeCountsEveryModel(t *testing.T) {
	// The top-level usage object leaves out the second model's requests.
	stream := `{"type":"system","subtype":"init","session_id":"session-a"}
{"type":"result","session_id":"session-a","total_cost_usd":0.3,` +
		`"usage":{"input_tokens":2,"output_tokens":4,"cache_creation_input_tokens":30},` +
		`"modelUsage":{"main":{"inputTokens":2,"outputTokens":4,"cacheReadInputTokens":50,"cacheCreationInputTokens":30,"costUSD":0.25},` +
		`"helper":{"inputTokens":5,"outputTokens":6,"cacheReadInputTokens":7,"cacheCreationInputTokens":8,"costUSD":0.05}}}`
	usage, err := ParseSchemaUsage("claude-code", strings.NewReader(stream))
	require.NoError(t, err)
	assert.Equal(t, &Usage{
		InputTokens: 7, OutputTokens: 10, CachedInputTokens: 57, CacheCreationTokens: 38,
		CostUSD: 0.3, HasCost: true, ThreadID: "session-a", UsageSource: "job_log_claude_schema",
	}, usage)
}

func TestParseSchemaUsageClaudeCountsEachRunOnce(t *testing.T) {
	one := map[string][4]int64{"main": {100, 10, 1000, 500}}
	two := map[string][4]int64{"main": {200, 20, 2000, 600}}
	for _, tc := range []struct {
		name   string
		stream []string
		want   Usage
	}{
		{
			name: "deltas within one session are summed",
			stream: []string{
				claudeResult("session-a", "model_usage_since_last_result", one),
				claudeResult("session-a", "model_usage_since_last_result", two),
			},
			want: Usage{
				InputTokens: 300, OutputTokens: 30, CachedInputTokens: 3000, CacheCreationTokens: 1100,
				CostUSD: 0.2, HasCost: true, ThreadID: "session-a",
			},
		},
		{
			name: "running totals within one session keep the last",
			stream: []string{
				claudeResult("session-a", "modelUsage", one),
				claudeResult("session-a", "modelUsage", two),
			},
			want: Usage{
				InputTokens: 200, OutputTokens: 20, CachedInputTokens: 2000, CacheCreationTokens: 600,
				CostUSD: 0.1, HasCost: true, ThreadID: "session-a",
			},
		},
		{
			name: "a wrapper's second CLI run is added",
			stream: []string{
				claudeResult("session-b", "model_usage_since_last_result", one),
				claudeResult("session-a", "modelUsage", two),
			},
			want: Usage{
				InputTokens: 300, OutputTokens: 30, CachedInputTokens: 3000, CacheCreationTokens: 1100,
				CostUSD: 0.2, HasCost: true, ThreadID: "session-a",
				ProviderSessionIDs: []string{"session-a", "session-b"}, ExpectedProviderSessions: 2,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage, err := ParseSchemaUsage("claude-code", strings.NewReader(strings.Join(tc.stream, "\n")))
			require.NoError(t, err)
			require.NotNil(t, usage)
			tc.want.UsageSource = "job_log_claude_schema"
			assert.InDelta(t, tc.want.CostUSD, usage.CostUSD, 1e-9)
			usage.CostUSD = tc.want.CostUSD
			assert.Equal(t, &tc.want, usage)
		})
	}
}

func TestParseSchemaUsageClaudeUnpricedRunLeavesTotalUnpriced(t *testing.T) {
	stream := claudeResult("session-a", "modelUsage", map[string][4]int64{"main": {1, 2, 3, 4}}) + "\n" +
		`{"type":"result","session_id":"session-b","modelUsage":{"main":{"inputTokens":10,"outputTokens":20}}}`
	usage, err := ParseSchemaUsage("claude-code", strings.NewReader(stream))
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, int64(11), usage.InputTokens)
	assert.Equal(t, int64(22), usage.OutputTokens)
	assert.False(t, usage.HasCost)
	assert.Zero(t, usage.CostUSD)
}

func TestParseJobLogUsageFileReadsTheLogOwnersFormat(t *testing.T) {
	dir := t.TempDir()
	claudeLog := filepath.Join(dir, "claude.log")
	require.NoError(t, os.WriteFile(claudeLog, []byte(
		claudeResult("session-a", "modelUsage", map[string][4]int64{"main": {1, 2, 3, 4}})+"\n"), 0o600))
	codexLog := filepath.Join(dir, "codex.log")
	require.NoError(t, os.WriteFile(codexLog, []byte(
		`{"type":"turn.completed","thread_id":"thread-a","usage":{"input_tokens":9,"output_tokens":8}}`+"\n"), 0o600))

	usage, err := ParseJobLogUsageFile("claude-code", claudeLog)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, int64(2), usage.OutputTokens)
	assert.Equal(t, "session-a", usage.ThreadID)

	usage, err = ParseJobLogUsageFile("codex", codexLog)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, int64(8), usage.OutputTokens)

	// Another agent's events in a log are not this agent's measured usage.
	usage, err = ParseJobLogUsageFile("codex", claudeLog)
	require.NoError(t, err)
	assert.Nil(t, usage)
	usage, err = ParseJobLogUsageFile("claude-code", codexLog)
	require.NoError(t, err)
	assert.Nil(t, usage)

	usage, err = ParseJobLogUsageFile("claude-code", filepath.Join(dir, "missing.log"))
	require.NoError(t, err)
	assert.Nil(t, usage)
}
