package prompt

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFixPlanEnvelope(t *testing.T) {
	for _, input := range []string{
		"roborev-fix-plan-v2\n{}",
		"roborev-fix-plan-v1\n{",
		"roborev-fix-plan-v1\n{}",
		"roborev-fix-plan-v1\n{\"planning_prompt\":\"plan\",\"implementation_prompt\":\"\"}",
	} {
		_, _, planned, err := DecodeFixPlan(input)
		require.Error(t, err, input)
		assert.True(t, planned)
	}
	plan, implement, planned, err := DecodeFixPlan("ordinary fix prompt")
	require.NoError(t, err)
	assert.False(t, planned)
	assert.Empty(t, plan)
	assert.Equal(t, "ordinary fix prompt", implement)
}

func TestDisplayFixPlanPrompt(t *testing.T) {
	assert.Equal(t, "ordinary fix prompt", DisplayFixPlanPrompt("ordinary fix prompt"))
	assert.Equal(t,
		"## Planning Prompt\n\nplanning context\n\n## Implementation Prompt\n\nimplementation prompt",
		DisplayFixPlanPrompt(EncodeFixPlan("planning context", "implementation prompt")),
	)

	invalid := "roborev-fix-plan-v1\nraw envelope payload"
	assert.Equal(t, "Fix plan prompt is unavailable because its stored envelope is invalid.", DisplayFixPlanPrompt(invalid))
	assert.NotContains(t, DisplayFixPlanPrompt(invalid), "raw envelope payload")
}

func FuzzFixPlanEnvelope(f *testing.F) {
	f.Add("Inspect cancellation", "Fix the worker")
	f.Add("Plan\nUnicode: 修正", "Findings with quotes: \"worker\"")
	f.Fuzz(func(t *testing.T, p, implementation string) {
		if !utf8.ValidString(p) || !utf8.ValidString(implementation) || strings.TrimSpace(p) == "" || strings.TrimSpace(implementation) == "" {
			t.Skip()
		}
		decodedPlan, decodedImplementation, planned, err := DecodeFixPlan(EncodeFixPlan(p, implementation))
		require.NoError(t, err)
		assert.True(t, planned)
		assert.Equal(t, p, decodedPlan)
		assert.Equal(t, implementation, decodedImplementation)
	})
}
