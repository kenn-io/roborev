package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLimitClassificationZeroValue(t *testing.T) {
	var c LimitClassification
	assert := assert.New(t)
	assert.Equal(LimitKindNone, c.Kind)
	assert.Empty(c.Agent)
	assert.True(c.ResetAt.IsZero())
	assert.Equal(time.Duration(0), c.CooldownFor)
	assert.Empty(c.Message)
}

func TestClassifyLimitProductionPatterns(t *testing.T) {
	// All nine substrings from the original isQuotaError set must
	// produce LimitKindQuota. This is the byte-for-byte regression
	// test for current Gemini and Codex detection.
	patterns := []string{
		"resource exhausted",
		"quota exceeded",
		"quota_exceeded",
		"quota exhausted",
		"quota_exhausted",
		"insufficient_quota",
		"exhausted your capacity",
		"capacity exhausted",
		"capacity_exhausted",
	}
	for _, p := range patterns {
		t.Run(p, func(t *testing.T) {
			cls := ClassifyLimit("gemini", "agent failed: "+p+", retrying...")
			assert := assert.New(t)
			assert.Equal(LimitKindQuota, cls.Kind, "expected LimitKindQuota for %q", p)
			assert.Equal("gemini", cls.Agent)
			assert.Contains(cls.Message, p)
		})
	}
}

func TestClassifyLimitExtractsCooldownDuration(t *testing.T) {
	cls := ClassifyLimit(
		"gemini",
		"You have exhausted your capacity on this model. Your quota will reset after 48m20s.",
	)
	assert := assert.New(t)
	assert.Equal(LimitKindQuota, cls.Kind)
	assert.Equal(48*time.Minute+20*time.Second, cls.CooldownFor)
	assert.True(cls.ResetAt.IsZero(), "no absolute time in this message")
}

func TestClassifyLimitNegativeCases(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"empty", ""},
		{"unrelated error", "exit status 1: file not found"},
		{"benign mention of limit", "limit set to 100 in config"},
		{"benign rate limit (transient, no rule produces it day 1)", "429 rate limit, retrying"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := ClassifyLimit("gemini", tc.msg)
			assert.Equal(t, LimitKindNone, cls.Kind, "expected LimitKindNone for %q", tc.msg)
		})
	}
}

func TestClassifyLimitTransientAndUsage(t *testing.T) {
	cases := []struct {
		name, agent, msg string
		want             LimitKind
	}{
		{
			"codex 429 retry limit", "codex",
			`codex stream reported failure: exceeded retry limit, last status: 429 Too Many Requests, request id: abc`,
			LimitKindTransient,
		},
		{
			"codex stream disconnect", "codex",
			`codex stream reported failure: Reconnecting... 2/5 (stream disconnected before completion: An error occurred while processing your request ... help.openai.com)`,
			LimitKindTransient,
		},
		{
			"gemini 429 capacity", "gemini",
			`Attempt 1 failed with status 429. No capacity available for model gemini-3.1-pro-preview on the server`,
			LimitKindTransient,
		},
		{"http 503", "codex", `agent: codex failed: 503 Service Unavailable`, LimitKindTransient},
		{
			"codex usage limit -> quota", "codex",
			`codex stream reported failure: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Mar 2nd, 2026 1:22 PM.`,
			LimitKindQuota,
		},
		{
			// Usage-cap text wrapped in a 429/Too Many Requests envelope must
			// still classify as quota: the codex usage-limit rule precedes the
			// generic transient rules, so first-match-wins keeps it quota. This
			// fails if the codex rule is moved back below the transient rules.
			"codex usage limit wrapped in 429 stays quota", "codex",
			`codex stream reported failure: You've hit your usage limit, last status: 429 Too Many Requests`,
			LimitKindQuota,
		},
		// Genuine/deterministic MUST NOT be transient:
		{"bare service unavailable not transient", "codex", `agent: codex failed: service unavailable`, LimitKindNone},
		{
			"model not supported", "codex",
			`codex stream reported failure: {"detail":"The 'devstral-2' model is not supported when using Codex with a ChatGPT account."}`,
			LimitKindNone,
		},
		{"unknown option", "droid", `agent: droid failed: error: unknown option '-C'`, LimitKindNone},
		{"stdin not a terminal", "codex", `agent: codex failed: Error: stdin is not a terminal`, LimitKindNone},
		{"context window", "codex", `Codex ran out of room in the model's context window.`, LimitKindNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassifyLimit(tc.agent, tc.msg).Kind)
		})
	}
}

func TestClassifyLimitWithRulesIsolatesSyntheticPattern(t *testing.T) {
	// Synthetic rule used only inside this test — does not pollute
	// defaultLimitRules.
	syntheticRules := []limitRule{
		{Agents: []string{"*"}, Substring: "test-claude session limit", Kind: LimitKindSession},
	}
	cls := classifyLimitWithRules(
		"claude-code",
		"5-hour test-claude session limit reached",
		syntheticRules,
	)
	assert := assert.New(t)
	assert.Equal(LimitKindSession, cls.Kind)
	assert.Equal("claude-code", cls.Agent)

	// Same message via the production ClassifyLimit must not match —
	// the synthetic rule is not in defaultLimitRules.
	cls2 := ClassifyLimit("claude-code", "5-hour test-claude session limit reached")
	assert.Equal(LimitKindNone, cls2.Kind, "synthetic rule must not leak into defaultLimitRules")
}
